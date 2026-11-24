package burnline

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming/types"
	openai "github.com/sashabaranov/go-openai"
)

// Transcriber turns one recording of a burn line into text.
type Transcriber interface {
	// AudioFormat is the Twilio recording format this transcriber wants:
	// "mp3" or "wav".
	AudioFormat() string
	// Transcribe returns the words of the recording. county is the display
	// name of the line's county, for providers that can be primed with it.
	Transcribe(ctx context.Context, audio []byte, county string) (string, error)
}

// Whisper transcribes with OpenAI's whisper-1.
type Whisper struct {
	Client *openai.Client
}

// AudioFormat implements Transcriber.
func (Whisper) AudioFormat() string { return "mp3" }

// Transcribe implements Transcriber. The prompt primes Whisper with the
// domain's proper nouns and jargon, which measurably improves "Calaveras",
// "Cal Fire" and the elevation phrasing.
func (w Whisper) Transcribe(ctx context.Context, audio []byte, county string) (string, error) {
	prompt := fmt.Sprintf("This is a recording of the %s burn information line. "+
		"The automated message covers burn day restrictions, fire danger, elevation "+
		"requirements, and permit information. Key terms: %s, Cal Fire, burn day, "+
		"permissive burn day, fire danger, elevation restrictions (3500 feet), "+
		"landscape debris, permits, burn barrel.", county, county)

	resp, err := w.Client.CreateTranscription(ctx, openai.AudioRequest{
		Model:       openai.Whisper1,
		FilePath:    "recording.mp3", // names the Reader's content; no file is opened
		Reader:      bytes.NewReader(audio),
		Prompt:      prompt,
		Temperature: 0.1,
		Language:    "en",
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

// StreamTranscriptionAPI is the one Amazon Transcribe call the reader makes
// (*transcribestreaming.Client satisfies it).
type StreamTranscriptionAPI interface {
	StartStreamTranscription(ctx context.Context, in *transcribestreaming.StartStreamTranscriptionInput, optFns ...func(*transcribestreaming.Options)) (*transcribestreaming.StartStreamTranscriptionOutput, error)
}

// AWSTranscribe transcribes with Amazon Transcribe's streaming API: the audio
// travels in the request and nothing is stored, so there is no bucket and no
// job to clean up. Credentials come from the client (the task role when
// hosted).
//
// It cannot be primed with a sentence the way Whisper can. Proper nouns come
// from a custom vocabulary instead (Vocabulary, created by the hosting
// account's infrastructure code); without one it writes "Tuolumne" as it
// hears it. The extraction step tolerates that, the published transcript
// shows it.
type AWSTranscribe struct {
	Client StreamTranscriptionAPI
	// Vocabulary is the name of a custom vocabulary in the client's Region,
	// or empty for none.
	Vocabulary string

	// stream replaces the network call in tests.
	stream func(ctx context.Context, pcm []byte, sampleRate int32) (string, error)
}

// AudioFormat implements Transcriber. Streaming takes raw PCM, which is what a
// WAV file carries; Twilio's WAV is the call's own 8 kHz 16-bit mono audio.
func (AWSTranscribe) AudioFormat() string { return "wav" }

// Transcribe implements Transcriber.
func (a AWSTranscribe) Transcribe(ctx context.Context, audio []byte, _ string) (string, error) {
	pcm, rate, err := wavPCM(audio)
	if err != nil {
		return "", err
	}
	stream := a.stream
	if stream == nil {
		stream = a.streamPCM
	}
	return stream(ctx, pcm, rate)
}

// pcmChunkSeconds is how much audio goes in one event. The service accepts a
// recording faster than real time; the chunk only bounds the event's size.
const pcmChunkSeconds = 0.5

func (a AWSTranscribe) streamPCM(ctx context.Context, pcm []byte, sampleRate int32) (string, error) {
	in := &transcribestreaming.StartStreamTranscriptionInput{
		LanguageCode:         types.LanguageCodeEnUs,
		MediaEncoding:        types.MediaEncodingPcm,
		MediaSampleRateHertz: aws.Int32(sampleRate),
	}
	if a.Vocabulary != "" {
		in.VocabularyName = aws.String(a.Vocabulary)
	}
	out, err := a.Client.StartStreamTranscription(ctx, in)
	if err != nil {
		return "", err
	}
	stream := out.GetStream()
	defer stream.Close()

	// 16-bit mono: two bytes a sample. Keep chunks on a sample boundary.
	chunk := int(float64(sampleRate)*pcmChunkSeconds) * 2
	sendErr := make(chan error, 1)
	go func() {
		defer stream.Writer.Close()
		for i := 0; i < len(pcm); i += chunk {
			end := min(i+chunk, len(pcm))
			ev := &types.AudioStreamMemberAudioEvent{Value: types.AudioEvent{AudioChunk: pcm[i:end]}}
			if err := stream.Writer.Send(ctx, ev); err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()

	// The service revises a sentence while it listens (partial results) and
	// then settles it. Only settled results are the transcript.
	var parts []string
	for ev := range stream.Events() {
		t, ok := ev.(*types.TranscriptResultStreamMemberTranscriptEvent)
		if !ok || t.Value.Transcript == nil {
			continue
		}
		for _, r := range t.Value.Transcript.Results {
			if r.IsPartial || len(r.Alternatives) == 0 {
				continue
			}
			parts = append(parts, aws.ToString(r.Alternatives[0].Transcript))
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	if err := <-sendErr; err != nil {
		return "", fmt.Errorf("sending audio: %w", err)
	}
	return strings.Join(parts, " "), nil
}

// wavPCM returns the samples of a 16-bit mono PCM WAV file and its sample
// rate. Anything else is refused: sent as PCM it would transcribe as noise,
// and an empty transcript looks like a silent line.
func wavPCM(b []byte) (pcm []byte, sampleRate int32, err error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, errors.New("recording is not a WAV file")
	}
	var haveFmt bool
	for i := 12; i+8 <= len(b); {
		id := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return nil, 0, errors.New("WAV fmt chunk is too short")
			}
			format := binary.LittleEndian.Uint16(b[body : body+2])
			channels := binary.LittleEndian.Uint16(b[body+2 : body+4])
			rate := binary.LittleEndian.Uint32(b[body+4 : body+8])
			bits := binary.LittleEndian.Uint16(b[body+14 : body+16])
			if format != 1 || channels != 1 || bits != 16 {
				return nil, 0, fmt.Errorf("WAV is format %d, %d channel(s), %d-bit; need 16-bit mono PCM", format, channels, bits)
			}
			sampleRate = int32(rate)
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, 0, errors.New("WAV data chunk comes before its fmt chunk")
			}
			// A recorder that cannot seek back leaves a wrong size; take what
			// is there.
			end := min(body+size, len(b))
			if end-body < 2 {
				return nil, 0, errors.New("WAV has no audio")
			}
			return b[body:end], sampleRate, nil
		}
		i = body + size + size%2
	}
	return nil, 0, errors.New("WAV has no data chunk")
}
