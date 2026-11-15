package search

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxQueryLen bounds a query string (in bytes). The endpoints are public and
// unauthenticated; a page of text is not a search.
const MaxQueryLen = 200

// maxTerms bounds how many terms one query may hold, for the same reason.
const maxTerms = 16

// Query is a parsed search: alternatives, any of which may match. Each
// alternative is a list of terms that must all hold.
//
// The syntax is what people already type into a search box: words are all
// required; "quoted words" must appear together in that order; a leading
// minus excludes; the word OR between terms starts an alternative. A road in
// any spelling ("hwy 4", "SR-4") is matched in every spelling.
type Query struct {
	Any []Group
}

// Group is one alternative: every term must hold.
type Group struct {
	Terms []Term
}

// Term is one word or one quoted phrase.
type Term struct {
	Text    string
	Phrase  bool // quoted: the words adjacent and in order
	Exclude bool // leading minus: must not appear
	// Prefix marks the last word of a query that is still being typed: it
	// also matches words that begin with it, so results narrow as someone
	// types ("floo" finds "flooded"). Never set on a phrase, an exclusion, a
	// road or a number.
	Prefix bool
}

// Errors ParseQuery returns. Both are the caller's mistake (HTTP 400).
var (
	ErrQueryTooLong = errors.New("q is too long")
	ErrQueryEmpty   = errors.New("q has nothing to search for")
)

// ParseQuery reads a search box's text. An empty or blank string is not a
// query: callers decide what no query means before calling.
//
// typing says the text is still being typed: its last word, when it is a
// plain word not followed by a space, also matches words that begin with it.
// Without it every word is matched whole, which is what a finished question
// (and a count) wants: "deer" must not count Deerfield Drive.
func ParseQuery(raw string, typing bool) (Query, error) {
	if len(raw) > MaxQueryLen {
		return Query{}, ErrQueryTooLong
	}
	if !utf8.ValidString(raw) {
		return Query{}, ErrQueryEmpty
	}
	raw = strings.Map(func(r rune) rune {
		switch r {
		case '“', '”', '„', '″':
			return '"'
		case 0: // PostgreSQL text cannot hold it
			return ' '
		}
		return r
	}, raw)

	var q Query
	cur := Group{}
	closeGroup := func() {
		if len(cur.Terms) > 0 {
			q.Any = append(q.Any, cur)
		}
		cur = Group{}
	}
	n := 0
	for _, tok := range tokenize(raw) {
		if !tok.phrase && !tok.exclude && strings.EqualFold(tok.text, "or") {
			closeGroup()
			continue
		}
		if !hasLetterOrDigit(tok.text) {
			continue
		}
		if tok.phrase {
			if n++; n > maxTerms {
				return Query{}, ErrQueryTooLong
			}
			cur.Terms = append(cur.Terms, Term{Text: tok.text, Phrase: true, Exclude: tok.exclude})
			continue
		}
		// A road outside quotes becomes its canonical token; one mention may
		// have been several words ("state route 4").
		for _, w := range strings.Fields(rewriteRoutes(tok.text)) {
			if n++; n > maxTerms {
				return Query{}, ErrQueryTooLong
			}
			cur.Terms = append(cur.Terms, Term{Text: strings.ToLower(w), Exclude: tok.exclude})
		}
	}
	closeGroup()

	// An alternative made only of exclusions matches nearly everything, which
	// is never what was meant: drop it.
	kept := q.Any[:0]
	for _, g := range q.Any {
		for _, t := range g.Terms {
			if !t.Exclude {
				kept = append(kept, g)
				break
			}
		}
	}
	q.Any = kept
	if len(q.Any) == 0 {
		return Query{}, ErrQueryEmpty
	}

	// The last thing typed, when it is a plain word, also matches as a
	// prefix. A trailing space means the word is finished.
	// "Last typed" means the final word of the input is that term: in
	// "fire or" the last word is the operator, and "fire" is finished.
	if words := strings.Fields(raw); typing && !unicode.IsSpace(rune(raw[len(raw)-1])) && len(words) > 0 {
		g := &q.Any[len(q.Any)-1]
		t := &g.Terms[len(g.Terms)-1]
		if !t.Phrase && !t.Exclude && prefixable(t.Text) && strings.EqualFold(words[len(words)-1], t.Text) {
			t.Prefix = true
		}
	}
	return q, nil
}

// Words returns the query's positive words, lowercased, phrases split. The
// in-memory matchers (places, cameras) use it; the store uses the terms.
func (q Query) Words() []string {
	var out []string
	for _, g := range q.Any {
		for _, t := range g.Terms {
			if !t.Exclude {
				out = append(out, strings.Fields(strings.ToLower(t.Text))...)
			}
		}
	}
	return out
}

// String is the query in its own syntax, normalised: what the server
// understood, echoed back so a caller can see how a road was read.
func (q Query) String() string {
	var groups []string
	for _, g := range q.Any {
		var terms []string
		for _, t := range g.Terms {
			s := t.Text
			if t.Phrase {
				s = `"` + s + `"`
			}
			if t.Exclude {
				s = "-" + s
			}
			terms = append(terms, s)
		}
		groups = append(groups, strings.Join(terms, " "))
	}
	return strings.Join(groups, " OR ")
}

type token struct {
	text            string
	phrase, exclude bool
}

// tokenize splits on spaces, keeping "quoted runs" whole. Adjacent road words
// are kept together ("state route 4" is one token) so the rewrite sees them:
// it yields the unquoted stretches between phrases as single tokens split
// later by rewriteRoutes + Fields, except that OR and exclusions need their
// own tokens, so a stretch is cut at those.
func tokenize(s string) []token {
	var out []token
	flush := func(words []string) {
		if len(words) > 0 {
			out = append(out, token{text: strings.Join(words, " ")})
		}
	}
	var run []string
	for len(s) > 0 {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			break
		}
		exclude := false
		rest := s
		if strings.HasPrefix(rest, "-") && len(rest) > 1 && !unicode.IsSpace(rune(rest[1])) {
			exclude = true
			rest = rest[1:]
		}
		if strings.HasPrefix(rest, `"`) {
			flush(run)
			run = nil
			body := rest[1:]
			end := strings.IndexByte(body, '"')
			if end < 0 { // an unclosed quote runs to the end
				end = len(body)
				s = ""
			} else {
				s = body[end+1:]
			}
			if p := strings.Join(strings.Fields(body[:end]), " "); p != "" {
				out = append(out, token{text: p, phrase: true, exclude: exclude})
			}
			continue
		}
		end := strings.IndexFunc(rest, func(r rune) bool { return unicode.IsSpace(r) || r == '"' })
		if end < 0 {
			end = len(rest)
		}
		word := rest[:end]
		s = rest[end:]
		switch {
		case exclude:
			flush(run)
			run = nil
			out = append(out, token{text: word, exclude: true})
		case strings.EqualFold(word, "or"):
			flush(run)
			run = nil
			out = append(out, token{text: word})
		default:
			run = append(run, word)
		}
	}
	flush(run)
	return out
}

func hasLetterOrDigit(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// prefixable: a plain word of letters and digits, at least two characters,
// not a road token and not a bare number ("route4" must not find route 49,
// nor "10" find 108).
func prefixable(w string) bool {
	if utf8.RuneCountInString(w) < 2 || IsRouteToken(w) {
		return false
	}
	letters := false
	for _, r := range w {
		switch {
		case unicode.IsLetter(r):
			letters = true
		case unicode.IsDigit(r):
		default:
			return false
		}
	}
	return letters
}
