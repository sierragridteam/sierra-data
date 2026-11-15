package search

import (
	"strings"
	"unicode"
)

// Matching small in-memory lists (places, cameras, scanner feeds, roads) with
// the same query syntax the store applies to events. These lists are a few
// hundred rows at most and live in memory, so they are matched here rather
// than indexed.

// Match reports whether fields satisfy the query and how well. fields are in
// order of importance (a name first, then where it is, then the rest); a
// match in an earlier field scores higher, and a query word that is a whole
// word of the name scores highest. Words match whole, or by prefix for the
// term being typed; a road matches in any spelling; names are not stemmed.
func (q Query) Match(fields ...string) (score int, ok bool) {
	toks := make([][]string, len(fields))
	for i, f := range fields {
		toks[i] = fieldTokens(f)
	}
	for _, g := range q.Any {
		s, matched := 0, true
		for _, t := range g.Terms {
			ts, hit := termScore(t, toks)
			if hit == t.Exclude {
				matched = false
				break
			}
			if !t.Exclude {
				s += ts
			}
		}
		if matched && (!ok || s > score) {
			score, ok = s, true
		}
	}
	return score, ok
}

// termScore is the best field a term matches in: the number of fields after
// it, plus one, doubled for a whole-word match in that field.
func termScore(t Term, toks [][]string) (int, bool) {
	words := plainTokens(t.Text)
	if len(words) == 0 {
		return 0, false
	}
	for i, field := range toks {
		base := len(toks) - i
		if t.Phrase || len(words) > 1 {
			// Adjacent and in order. A hyphenated word ("west-1w") is several
			// tokens and is matched the same way.
			if containsRun(field, words) {
				return 2 * base, true
			}
			continue
		}
		w := words[0]
		for _, tok := range field {
			if tok == w {
				return 2 * base, true
			}
		}
		if t.Prefix {
			for _, tok := range field {
				if strings.HasPrefix(tok, w) {
					return base, true
				}
			}
		}
	}
	return 0, false
}

func containsRun(field, words []string) bool {
	for i := 0; i+len(words) <= len(field); i++ {
		ok := true
		for j, w := range words {
			if field[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// fieldTokens splits text into lowercase words of letters and digits, and
// adds the canonical token of every road it mentions, so "SR-4" in a camera's
// route is found by "hwy 4" (which the query holds as "route4").
func fieldTokens(s string) []string {
	return append(plainTokens(s), RouteTokens(s)...)
}

func plainTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
