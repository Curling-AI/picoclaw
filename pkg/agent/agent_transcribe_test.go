package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

type failingTranscriber struct{}

func (failingTranscriber) Name() string { return "failing" }

func (failingTranscriber) Transcribe(context.Context, string) (*asr.TranscriptionResponse, error) {
	return nil, errors.New("gateway down")
}

func TestTranscribeAudioInMessage_UntranscribedAudioTellsTheAgentNotToImprovise(t *testing.T) {
	cases := map[string]asr.Transcriber{
		"no transcriber":     nil,
		"transcriber fails":  failingTranscriber{},
		"no speech in audio": &fixedTranscriber{text: ""},
	}
	for name, transcriber := range cases {
		t.Run(name, func(t *testing.T) {
			store := media.NewFileMediaStore()
			audioPath := filepath.Join(t.TempDir(), "voice.ogg")
			if err := os.WriteFile(audioPath, []byte("fake audio"), 0o644); err != nil {
				t.Fatalf("write audio fixture: %v", err)
			}
			ref, err := store.Store(audioPath, media.MediaMeta{
				Filename:      "voice.ogg",
				ContentType:   "audio/ogg",
				CleanupPolicy: media.CleanupPolicyForgetOnly,
			}, "scope-voice")
			if err != nil {
				t.Fatalf("store audio fixture: %v", err)
			}
			al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, transcriber: transcriber}

			msg, hadAudio := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
				Content: "transcreva na íntegra [audio: voice.ogg]",
				Media:   []string{ref},
			})

			if !hadAudio {
				t.Fatal("expected hadAudio, or the deferred placeholder is never sent")
			}
			if want := "transcreva na íntegra " + untranscribedAudioNote; msg.Content != want {
				t.Fatalf("content = %q, want %q", msg.Content, want)
			}
			if audioPlaceholderRegex.MatchString(msg.Content) {
				t.Fatal("note matches audioPlaceholderRegex and would be overwritten by the path tag")
			}
			if len(msg.Media) != 1 || msg.Media[0] != ref {
				t.Fatalf("media = %v, want the audio kept for non-transcription work", msg.Media)
			}
		})
	}
}

// A .mp3 sent as a document carries no [audio] annotation; it still has to get
// the note, appended, with the file kept for "save it to my Drive" requests.
func TestTranscribeAudioInMessage_PlainAudioAttachmentGetsNoteAppended(t *testing.T) {
	store := media.NewFileMediaStore()
	audioPath := filepath.Join(t.TempDir(), "musica.mp3")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	ref, err := store.Store(audioPath, media.MediaMeta{Filename: "musica.mp3", ContentType: "audio/mpeg"}, "scope")
	if err != nil {
		t.Fatalf("store audio fixture: %v", err)
	}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store}

	msg, _ := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "salva no meu Drive",
		Media:   []string{ref},
	})

	if want := "salva no meu Drive\n" + untranscribedAudioNote; msg.Content != want {
		t.Fatalf("content = %q, want %q", msg.Content, want)
	}
	if len(msg.Media) != 1 || msg.Media[0] != ref {
		t.Fatalf("media = %v, want the attachment kept", msg.Media)
	}
}

func TestTranscribeAudioInMessage_TranscribedAudioBecomesVoiceText(t *testing.T) {
	store := media.NewFileMediaStore()
	audioPath := filepath.Join(t.TempDir(), "voice.ogg")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	ref, err := store.Store(audioPath, media.MediaMeta{Filename: "voice.ogg", ContentType: "audio/ogg"}, "scope")
	if err != nil {
		t.Fatalf("store audio fixture: %v", err)
	}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, transcriber: &fixedTranscriber{text: "olá"}}

	msg, _ := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "[voice]",
		Media:   []string{ref},
	})

	if msg.Content != "[voice: olá]" || len(msg.Media) != 0 || strings.Contains(msg.Content, "not transcribed") {
		t.Fatalf("got content %q media %v", msg.Content, msg.Media)
	}
}
