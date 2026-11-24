package burnline

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-data/internal/clients/twilio"
	"github.com/sierragridteam/sierra-data/internal/lib/llm"
)

// wav builds a PCM WAV file. extra is a chunk placed between fmt and data, as
// Twilio's recorder and most others do ("LIST").
func wav(format, channels uint16, rate uint32, bits uint16, extra, samples []byte) []byte {
	var b bytes.Buffer
	chunk := func(id string, body []byte) {
		b.WriteString(id)
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
		b.Write(body)
		if len(body)%2 == 1 {
			b.WriteByte(0)
		}
	}
	b.WriteString("RIFF\x00\x00\x00\x00WAVE")
	var f bytes.Buffer
	_ = binary.Write(&f, binary.LittleEndian, format)
	_ = binary.Write(&f, binary.LittleEndian, channels)
	_ = binary.Write(&f, binary.LittleEndian, rate)
	_ = binary.Write(&f, binary.LittleEndian, rate*uint32(channels)*uint32(bits)/8)
	_ = binary.Write(&f, binary.LittleEndian, channels*bits/8)
	_ = binary.Write(&f, binary.LittleEndian, bits)
	chunk("fmt ", f.Bytes())
	if extra != nil {
		chunk("LIST", extra)
	}
	chunk("data", samples)
	return b.Bytes()
}

func TestWavPCM(t *testing.T) {
	samples := []byte{1, 0, 2, 0, 3, 0, 4, 0}

	pcm, rate, err := wavPCM(wav(1, 1, 8000, 16, nil, samples))
	require.NoError(t, err)
	assert.Equal(t, samples, pcm)
	assert.Equal(t, int32(8000), rate)

	// A chunk of odd length before the data is skipped with its pad byte.
	pcm, _, err = wavPCM(wav(1, 1, 8000, 16, []byte("abc"), samples))
	require.NoError(t, err)
	assert.Equal(t, samples, pcm)

	// A data size larger than the file (a recorder that never went back to
	// write it) yields what is there.
	short := wav(1, 1, 8000, 16, nil, samples)
	pcm, _, err = wavPCM(short[:len(short)-2])
	require.NoError(t, err)
	assert.Equal(t, samples[:6], pcm)
}

// Sent as PCM, anything else would come back as noise or silence, and an
// empty transcript reads as a silent line. Refuse it by name instead.
func TestWavPCMRefusesWhatIsNotMono16BitPCM(t *testing.T) {
	samples := []byte{1, 0, 2, 0}
	for name, tc := range map[string]struct {
		audio []byte
		want  string
	}{
		"an mp3":       {[]byte("ID3\x04\x00\x00\x00\x00\x00\x00 not a wav at all"), "not a WAV"},
		"empty":        {nil, "not a WAV"},
		"stereo":       {wav(1, 2, 8000, 16, nil, samples), "2 channel"},
		"8-bit":        {wav(1, 1, 8000, 8, nil, samples), "8-bit"},
		"mu-law":       {wav(7, 1, 8000, 8, nil, samples), "format 7"},
		"no samples":   {wav(1, 1, 8000, 16, nil, nil), "no audio"},
		"no data":      {wav(1, 1, 8000, 16, nil, samples)[:36], "no data chunk"},
		"riff, no fmt": {[]byte("RIFF\x00\x00\x00\x00WAVEdata\x04\x00\x00\x00\x01\x00\x02\x00"), "before its fmt"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := wavPCM(tc.audio)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestAWSTranscribeStreamsTheSamplesAtTheFilesRate(t *testing.T) {
	samples := bytes.Repeat([]byte{7, 0}, 100)
	var gotPCM []byte
	var gotRate int32
	a := AWSTranscribe{stream: func(_ context.Context, pcm []byte, rate int32) (string, error) {
		gotPCM, gotRate = pcm, rate
		return "today is a burn day", nil
	}}
	assert.Equal(t, "wav", a.AudioFormat())

	text, err := a.Transcribe(context.Background(), wav(1, 1, 8000, 16, nil, samples), "Calaveras County")
	require.NoError(t, err)
	assert.Equal(t, "today is a burn day", text)
	assert.Equal(t, samples, gotPCM)
	assert.Equal(t, int32(8000), gotRate)

	_, err = a.Transcribe(context.Background(), []byte("fake-mp3"), "Calaveras County")
	require.ErrorContains(t, err, "not a WAV")
}

type cannedJSON string

func (c cannedJSON) CompleteJSON(context.Context, llm.JSONRequest) (string, error) {
	return string(c), nil
}

// The reader asks Twilio for the format its transcriber wants: with Amazon
// Transcribe that is the WAV, not the mp3 Whisper takes.
func TestRead_DownloadsTheTranscribersFormat(t *testing.T) {
	var recordingPath string
	tw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/Calls.json"):
			_, _ = w.Write([]byte(`{"sid":"CA1","status":"queued"}`))
		case strings.Contains(r.URL.Path, "/Recordings/"):
			recordingPath = r.URL.Path
			_, _ = w.Write(wav(1, 1, 8000, 16, nil, []byte{1, 0, 2, 0}))
		case strings.Contains(r.URL.Path, "/Recordings.json"):
			_, _ = w.Write([]byte(`{"recordings":[{"sid":"RE1","duration":"40"}]}`))
		case strings.Contains(r.URL.Path, "/Calls/CA1.json"):
			_, _ = w.Write([]byte(`{"sid":"CA1","status":"completed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer tw.Close()

	r := NewReader(
		twilio.NewClientWithHTTPDoer(tw.URL, "AC1", "tok", tw.Client()),
		AWSTranscribe{stream: func(context.Context, []byte, int32) (string, error) {
			return "Today is a burn day in Tuwame County.", nil
		}},
		cannedJSON(`{"status":"green","message":"Today is a burn day.","cleanedTranscription":"Today is a burn day in Tuolumne County.","confidence":96}`),
		Config{Phone: "+12095335598", From: "+15550000000", TwiMLURL: "https://twiml.test",
			County: "Tuolumne County", HangUpAfter: time.Millisecond, Model: "deepseek.v3.2"},
	)
	r.sleep = func(time.Duration) {}
	r.poll, r.settle = 0, 0

	got, err := r.Read(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusBurn, got.Status)
	assert.Equal(t, "Today is a burn day in Tuolumne County.", got.Transcript)
	assert.True(t, strings.HasSuffix(recordingPath, "/Recordings/RE1.wav"), recordingPath)
}

type recordingJSON struct {
	prompt string
	answer string
}

func (c *recordingJSON) CompleteJSON(_ context.Context, req llm.JSONRequest) (string, error) {
	c.prompt = req.Prompt
	return c.answer, nil
}

// "Today" in the prompt is the district's day. At 5 pm Pacific a host on UTC
// is already on the next date, and a model told that date reads a message
// for today as out of date (seen 2026-11-23).
func TestReadRecording_TodayIsTheDistrictsDay(t *testing.T) {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	require.NoError(t, err)
	ai := &recordingJSON{answer: `{"status":"green","message":"Burn day.","cleanedTranscription":"Monday is a burn day.","confidence":90}`}
	stt := AWSTranscribe{stream: func(context.Context, []byte, int32) (string, error) { return "Monday is a burn day.", nil }}
	audio := wav(1, 1, 8000, 16, nil, []byte{1, 0})

	r := NewReader(nil, stt, ai, Config{County: "Tuolumne County", Location: pacific})
	r.now = func() time.Time { return time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC) } // 5 pm Monday, Pacific
	_, err = r.ReadRecording(context.Background(), audio)
	require.NoError(t, err)
	assert.Contains(t, ai.prompt, "Today's date: Monday, November 23, 2026")

	// A saved recording is read as of the day it was made.
	r = NewReader(nil, stt, ai, Config{County: "Tuolumne County", Location: pacific,
		Today: time.Date(2026, 11, 6, 12, 0, 0, 0, pacific)})
	_, err = r.ReadRecording(context.Background(), audio)
	require.NoError(t, err)
	assert.Contains(t, ai.prompt, "Today's date: Friday, November 6, 2026")
}
