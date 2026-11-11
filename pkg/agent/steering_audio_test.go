package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// writeJoinedAudio is a web upload: an absolute workspace path, not a
// media:// ref, which is what SteerSession gets from ethos.
func writeJoinedAudio(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(path, []byte("fake audio"), 0o644); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	return path
}

type countingTranscriber struct {
	mu   sync.Mutex
	n    int
	text string
}

func (c *countingTranscriber) Name() string { return "counting" }

func (c *countingTranscriber) Transcribe(context.Context, string) (*asr.TranscriptionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return &asr.TranscriptionResponse{Text: c.text}, nil
}

func (c *countingTranscriber) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func joinedTurn() *turnState { return &turnState{channel: "grpc", chatID: "run-a"} }

// A message SteerSession queued gets what one that opens a turn gets: its
// audio transcribed, or the note that replaces it. The slice the caller holds
// keeps the message as sent, which is what the screen places.
func TestTranscribeJoinedAudio_MarkedAudio(t *testing.T) {
	cases := map[string]struct {
		transcriber asr.Transcriber
		content     string
		keepsAudio  bool
	}{
		"transcribed":    {&fixedTranscriber{text: "manda o resumo"}, "[voice: manda o resumo]", false},
		"no transcriber": {nil, noTranscriberAudioNote, true},
		"fails":          {failingTranscriber{}, failedTranscriptionAudioNote, true},
		"no speech":      {&fixedTranscriber{text: ""}, noSpeechAudioNote, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore(), transcriber: tc.transcriber}
			audio := writeJoinedAudio(t)
			sent := []providers.Message{{Role: "user", Content: "[voice]", Media: []string{audio}, TranscribeAudio: true}}

			got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

			if got[0].Content != tc.content {
				t.Errorf("content = %q, want %q", got[0].Content, tc.content)
			}
			if kept := len(got[0].Media) == 1; kept != tc.keepsAudio {
				t.Errorf("media = %v, keeps audio = %v, want %v", got[0].Media, kept, tc.keepsAudio)
			}
			if got[0].TranscribeAudio {
				t.Error("the mark stayed on: a later read would transcribe it again")
			}
			if sent[0].Content != "[voice]" || len(sent[0].Media) != 1 || !sent[0].TranscribeAudio {
				t.Errorf("the message as sent changed: %+v", sent[0])
			}
		})
	}
}

// A channel message was transcribed before it was queued (agent.go:252). One
// whose audio failed keeps the file next to the note: read again, it would get
// a second note and a second charge.
func TestTranscribeJoinedAudio_LeavesUnmarkedMessagesAlone(t *testing.T) {
	tr := &countingTranscriber{text: "não devia rodar"}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore(), transcriber: tr}
	audio := writeJoinedAudio(t)
	sent := []providers.Message{{Role: "user", Content: "fala\n" + failedTranscriptionAudioNote, Media: []string{audio}}}

	got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

	if tr.calls() != 0 {
		t.Errorf("transcribed %d times an unmarked message", tr.calls())
	}
	if got[0].Content != sent[0].Content || len(got[0].Media) != 1 {
		t.Errorf("message changed: %+v", got[0])
	}
}

// An image or a document joins as it did: SteerSession queues the file.
func TestTranscribeJoinedAudio_MarkedWithoutAudio(t *testing.T) {
	tr := &countingTranscriber{text: "não devia rodar"}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore(), transcriber: tr}
	photo := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(photo, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sent := []providers.Message{{Role: "user", Content: "olha isso", Media: []string{photo}, TranscribeAudio: true}}

	got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

	if tr.calls() != 0 || got[0].Content != "olha isso" || len(got[0].Media) != 1 || got[0].Media[0] != photo {
		t.Errorf("an image changed or was transcribed: %+v (%d calls)", got[0], tr.calls())
	}
}

// Only a message with files is marked: a text has nothing to transcribe.
func TestSteerSession_MarksMessagesWithFiles(t *testing.T) {
	al := newSessionSteeringLoop(t, newSessionRecordingProvider("", ""))
	if err := al.SteerSession(conversationA, "só texto", nil); err != nil {
		t.Fatal(err)
	}
	if err := al.SteerSession(conversationA, "[voice]", []string{"/workspace/uploads/voice.ogg"}); err != nil {
		t.Fatal(err)
	}
	var queued []providers.Message
	for {
		got := al.dequeueSteeringMessagesForScope(conversationA)
		if len(got) == 0 {
			break
		}
		queued = append(queued, got...)
	}
	if len(queued) != 2 {
		t.Fatalf("queued %d messages, want 2", len(queued))
	}
	if queued[0].TranscribeAudio || !queued[1].TranscribeAudio {
		t.Fatalf("marks = %v, want text unmarked and file marked", []bool{queued[0].TranscribeAudio, queued[1].TranscribeAudio})
	}
}
