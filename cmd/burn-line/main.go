// Command burn-line calls the configured county burn-information lines, extracts
// today's burn-day status from each, and PUSHES the readings to The Grid's
// shared push-ingest endpoint as the `burn.line` stream.
//
// It runs on a schedule, not inside the server: placing phone calls costs
// money and must not be re-triggered by a container restart, so the dialing
// lives somewhere with exactly-once-per-day semantics. The server only ever
// receives the results. A scheduled task running the Dockerfile's `burn-line`
// image starts it (-not-before=06:00, with later retries);
// .github/workflows/burn-line.yml runs it by hand.
//
// # prefab.yaml is the single source of truth
//
// Which lines exist, which are dialed, and which counties each speaks for all
// come from `grid.burn.lines` — the same config the server reads. The workflow
// does NOT enumerate numbers, so enabling a county is one flag in one file
// rather than a config edit plus a matching CI edit that can silently drift
// (config-on/CI-off degrades the source with no reading; CI-on/config-off gets
// 400s from the endpoint).
//
// Usage:
//
//	burn-line -endpoint https://data.sierragridteam.org/api/v1/ingest/burn.line
//	burn-line -line calaveras-apcd      # just one line
//	burn-line -dry-run                  # call and extract, print, do not push
//	burn-line -not-before 06:00 ...     # scheduled mode: skip lines already
//	                                    # read this morning; before 06:00
//	                                    # Pacific, dial nothing
//	burn-line -provider aws ...         # Amazon Transcribe and a Bedrock model
//	                                    # instead of OpenAI
//	burn-line -line tuolumne-apcd -recording call.wav
//	                                    # read a saved recording: no call, no
//	                                    # push. How a provider is tried.
//
// Credentials come from the environment:
//
//	TWILIO_ACCOUNT_SID, TWILIO_AUTH_TOKEN, TWILIO_FROM_NUMBER, TWILIO_TWIML_URL
//	OPENAI_API_KEY      with -provider=openai (the default)
//	AWS credentials and AWS_REGION, however the SDK finds them, with
//	                    -provider=aws: bedrock:InvokeModel on the model and
//	                    transcribe:StartStreamTranscription
//	GRID_INGEST_TOKEN   a grid.ingest reporter token granted the "burn.line"
//	                    stream. Mint with:
//	                      make ingest-token REPORTER=burn-line STREAMS=burn.line
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sierragridteam/sierra-data/internal/clients/burnline"
	"github.com/sierragridteam/sierra-data/internal/clients/twilio"
	"github.com/sierragridteam/sierra-data/internal/config"
)

func main() {
	var (
		endpoint  = flag.String("endpoint", "", "Grid burn.line ingest endpoint, e.g. https://host/api/v1/ingest/burn.line (required unless -dry-run)")
		only      = flag.String("line", "", "dial only this line id (default: every enabled line)")
		provider  = flag.String("provider", providerOpenAI, "who transcribes and extracts: openai (Whisper and a chat model) or aws (Amazon Transcribe and a Bedrock model)")
		model     = flag.String("model", "", "model for extraction (default: gpt-4o for openai, deepseek.v3.2 for aws)")
		vocab     = flag.String("vocabulary", "", "with -provider=aws: name of an Amazon Transcribe custom vocabulary")
		asOf      = flag.String("as-of", "", "with -recording: the day the recording was made, YYYY-MM-DD, so its dates are checked against that day and not today")
		recording = flag.String("recording", "", "read this saved recording as the line given by -line instead of calling it; prints the reading, never pushes (wav for aws, mp3 for openai)")
		hangUp    = flag.Duration("hangup-after", 90*time.Second, "how long to stay on each line")
		timeout   = flag.Duration("timeout", 10*time.Minute, "deadline per line")
		dryRun    = flag.Bool("dry-run", false, "call and extract, print the readings, do not push")
		attempts  = flag.Int("attempts", 3, "tries per line before giving up on it")
		retryGap  = flag.Duration("retry-wait", 2*time.Minute, "wait between tries of one line")
		notBefore = flag.String("not-before", "", "scheduled mode, Pacific HH:MM: dial nothing before it, and skip lines already read since it today")
	)
	flag.Parse()

	if *model == "" {
		*model = defaultModel(*provider)
	}
	if *recording != "" {
		if *only == "" {
			fatal("-recording needs -line: which line is this a recording of")
		}
		if *notBefore != "" {
			fatal("-recording reads a file whatever the day's readings are; it does not go with -not-before")
		}
		*dryRun = true
	} else if *asOf != "" {
		fatal("-as-of goes with -recording: a call is always read as of now")
	}
	if *endpoint == "" && !*dryRun {
		fatal("-endpoint is required (or pass -dry-run)")
	}

	cfg := config.LoadConfig()
	lines := cfg.Grid.Burn.EnabledLines()
	if *only != "" {
		l, ok := cfg.Grid.Burn.Line(*only)
		if !ok {
			fatal("no line %q in grid.burn.lines", *only)
		}
		if !l.Dialed() {
			fatal("line %q is configured but not enabled (set enabled: true to dial it)", *only)
		}
		lines = []config.BurnLine{l}
	}
	if len(lines) == 0 {
		fatal("no enabled burn lines in grid.burn.lines")
	}

	// Scheduled mode. The schedule fires more than once a morning (retries);
	// this turns all those runs into one morning read per line.
	if *notBefore != "" {
		start, err := dayStart(time.Now(), *notBefore)
		if err != nil {
			fatal("%v", err)
		}
		if time.Now().Before(start) {
			fmt.Printf("Before %s Pacific (%s): too early to read today's line, nothing to do.\n",
				*notBefore, start.Format(time.RFC3339))
			return
		}
		if *endpoint == "" {
			fatal("-not-before needs -endpoint, to see which lines were already read")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		observed, err := lineObservations(ctx, *endpoint)
		cancel()
		if err != nil {
			// Not knowing costs at most a duplicate call, while skipping would
			// cost the day's reading. Dial.
			fmt.Fprintf(os.Stderr, "could not check existing readings, dialing every line: %v\n", err)
		}
		var due []config.BurnLine
		for _, l := range lines {
			if at, ok := observed[strings.ToLower(l.ID)]; ok && !at.Before(start) {
				fmt.Printf("%s: already read today at %s, skipping\n", l.ID, at.In(pacific).Format("15:04 MST"))
				continue
			}
			due = append(due, l)
		}
		if len(due) == 0 {
			fmt.Printf("Every line already read today, nothing to do.\n")
			return
		}
		lines = due
	}

	if *recording != "" {
		readRecording(lines[0], *recording, *asOf, *provider, *vocab, *model, *timeout)
		return
	}

	sid := mustEnv("TWILIO_ACCOUNT_SID")
	authToken := mustEnv("TWILIO_AUTH_TOKEN")
	from := mustEnv("TWILIO_FROM_NUMBER")
	twiml := mustEnv("TWILIO_TWIML_URL")
	ms, err := newModels(context.Background(), *provider, *vocab)
	if err != nil {
		fatal("%v", err)
	}
	var ingestToken string
	if !*dryRun {
		ingestToken = mustEnv("GRID_INGEST_TOKEN")
	}

	// Check the endpoint will take our report BEFORE dialing. Every call costs
	// Twilio and model money, and a reading that cannot be delivered is wasted,
	// so a bad token, a missing grant, or a server that is down must fail here,
	// for free.
	if !*dryRun {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := preflight(ctx, *endpoint, ingestToken)
		cancel()
		if err != nil {
			fatal("preflight failed, not dialing: %v", err)
		}
	}

	tw := twilio.NewClient(sid, authToken)

	// Lines are independent: one being unreachable must not stop the others.
	// Every failure is reported and the exit code reflects whether ANY failed,
	// so a single bad line is visible in CI without losing the good ones.
	// Read every line first, then push ONE report. Reading is independent per
	// line — one unreachable recording must not cost the others — but the push
	// is batched, because the endpoint rate-limits per reporter against the last
	// ACCEPTED report, so a push per line would 429 everything after the first.
	var failed []string
	var readings []pushReading
	for _, l := range lines {
		fmt.Printf("\n=== %s (%s) — %s\n", l.ID, l.Phone, strings.Join(l.Counties, ", "))
		r, err := readWithRetry(l, tw, ms, runOpts{
			from: from, twiml: twiml, model: *model,
			hangUp: *hangUp, timeout: *timeout,
		}, *attempts, *retryGap)
		if err != nil {
			failed = append(failed, l.ID)
			continue
		}
		readings = append(readings, pushReading{
			LineID:               l.ID,
			Status:               string(r.Status),
			Message:              r.Message,
			CleanedTranscription: r.Transcript,
			Confidence:           r.Confidence,
			ObservedAt:           r.ObservedAt.Format(time.RFC3339),
		})
	}

	if len(readings) > 0 && !*dryRun {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		if err := push(ctx, *endpoint, ingestToken, readings); err != nil {
			cancel()
			fatal("push failed (%d reading(s) not delivered): %v", len(readings), err)
		}
		cancel()
		fmt.Printf("\nPushed %d reading(s) to %s\n", len(readings), *endpoint)
	} else if *dryRun {
		fmt.Printf("\n-dry-run: %d reading(s) not pushed\n", len(readings))
	}

	if len(failed) > 0 {
		// A failed line contributes NOTHING to the report. Its previous reading
		// then ages past the server's freshness gate and the facet reads UNKNOWN —
		// the honest outcome, and why this exits non-zero rather than guessing.
		fatal("%d of %d line(s) failed: %s", len(failed), len(lines), strings.Join(failed, ", "))
	}
	fmt.Printf("\nAll %d line(s) read successfully.\n", len(lines))
}

// runOpts bundles the per-run settings shared by every line.
type runOpts struct {
	from, twiml, model string
	hangUp, timeout    time.Duration
}

// readWithRetry reads one line, retrying any failure.
//
// Retried blindly rather than by error class: the common failure is the line
// answering "busy" (other callers, the same morning), but a dropped call, an
// empty recording or a transcription or model blip are equally transient, and every
// failure mode is a few cents at most. A line that fails every attempt is left
// for the next scheduled run.
func readWithRetry(l config.BurnLine, tw *twilio.Client, ms models, o runOpts, attempts int, wait time.Duration) (*burnline.Reading, error) {
	if attempts < 1 {
		attempts = 1
	}
	var err error
	for i := 1; i <= attempts; i++ {
		var r *burnline.Reading
		if r, err = read(l, tw, ms, o); err == nil {
			return r, nil
		}
		fmt.Fprintf(os.Stderr, "  attempt %d/%d FAILED: %v\n", i, attempts, err)
		if i < attempts {
			fmt.Printf("  retrying in %s\n", wait)
			time.Sleep(wait)
		}
	}
	return nil, err
}

// read dials one line and returns its reading.
func read(l config.BurnLine, tw *twilio.Client, ms models, o runOpts) (*burnline.Reading, error) {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	reader := burnline.NewReader(tw, ms.stt, ms.ai, burnline.Config{
		Phone: l.Phone, From: o.from, TwiMLURL: o.twiml,
		County: displayName(l), HangUpAfter: o.hangUp, Model: o.model,
		Location: pacific,
	})

	reading, err := reader.Read(ctx)
	if err != nil {
		return nil, err
	}

	printReading(reading, 240)
	return reading, nil
}

func printReading(reading *burnline.Reading, transcriptChars int) {
	fmt.Printf("  status:     %s\n", reading.Status)
	fmt.Printf("  confidence: %d\n", reading.Confidence)
	fmt.Printf("  message:    %s\n", reading.Message)
	fmt.Printf("  observedAt: %s\n", reading.ObservedAt.Format(time.RFC3339))
	fmt.Printf("  transcript: %s\n", truncate(reading.Transcript, transcriptChars))
}

// readRecording reads a saved recording as line l with the given provider and
// prints the reading, transcript in full. It places no call, needs no Twilio
// or ingest credential, and pushes nothing: it exists to try a provider, a
// model or a vocabulary on audio we already have.
func readRecording(l config.BurnLine, path, asOf, provider, vocabulary, model string, timeout time.Duration) {
	audio, err := os.ReadFile(path)
	if err != nil {
		fatal("%v", err)
	}
	var today time.Time
	if asOf != "" {
		day, err := time.ParseInLocation("2006-01-02", asOf, pacific)
		if err != nil {
			fatal("-as-of must be YYYY-MM-DD: %v", err)
		}
		today = day.Add(12 * time.Hour)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ms, err := newModels(ctx, provider, vocabulary)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("=== %s from %s (%s, %s)\n", l.ID, path, provider, model)
	reader := burnline.NewReader(nil, ms.stt, ms.ai, burnline.Config{
		County: displayName(l), Model: model, Location: pacific, Today: today,
	})
	reading, err := reader.ReadRecording(ctx, audio)
	if err != nil {
		fatal("%v", err)
	}
	printReading(reading, 1<<20)
	fmt.Printf("\n-recording: not pushed\n")
}

// pushBody mirrors the burn.line stream contract. Request bodies on
// /api/v1/ingest are snake_case (responses are camelCase).
type pushBody struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Readings      []pushReading `json:"readings"`
}

type pushReading struct {
	LineID               string `json:"line_id"`
	Status               string `json:"status"`
	Message              string `json:"message"`
	CleanedTranscription string `json:"cleaned_transcription"`
	Confidence           int32  `json:"confidence"`
	ObservedAt           string `json:"observed_at"`
}

// push sends every reading in ONE report.
//
// Batched deliberately: the endpoint rate-limits per reporter against the last
// ACCEPTED report (60s for this one), so pushing each line separately would 429
// every line after the first. One report is also atomic from the operator's
// point of view — the 202 body reports accepted counts and per-reading warnings.
func push(ctx context.Context, endpoint, token string, readings []pushReading) error {
	body, err := json.Marshal(pushBody{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Readings:      readings,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	// Warnings mean some readings were skipped; surface them, they are the only
	// signal that a line silently did not land.
	fmt.Printf("  server: %s\n", strings.TrimSpace(buf.String()))
	return nil
}

// preflight asks the endpoint whether it would accept a report from this token
// (`?preflight=true` answers 204 after authenticating and checking the stream
// grant, recording nothing).
func preflight(ctx context.Context, endpoint, token string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	q := u.Query()
	q.Set("preflight", "true")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(buf.String()))
	}
	return nil
}

// displayName is what the transcription and extraction prompts are primed with.
// The line's own name is best ("Calaveras County APCD burn line"); fall back to
// the first county it speaks for.
func displayName(l config.BurnLine) string {
	if l.Name != "" {
		return l.Name
	}
	if len(l.Counties) > 0 {
		return countyDisplayName(l.Counties[0])
	}
	return l.ID
}

// countyDisplayName turns "calaveras-county" into "Calaveras County".
func countyDisplayName(slug string) string {
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func mustEnv(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		fatal("%s is required in the environment", key)
	}
	return v
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "burn-line: "+format+"\n", args...)
	os.Exit(1)
}
