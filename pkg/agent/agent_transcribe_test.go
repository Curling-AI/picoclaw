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

// countingChannelManager counts what the agent sends on its own.
type countingChannelManager struct {
	recordingChannelManager
	placeholders int
	messages     int
}

func (m *countingChannelManager) SendPlaceholder(context.Context, string, string) bool {
	m.placeholders++
	return true
}

func (m *countingChannelManager) SendMessage(context.Context, bus.OutboundMessage) error {
	m.messages++
	return nil
}

func storeAudio(t *testing.T, store media.MediaStore, filename, contentType string) string {
	t.Helper()
	audioPath := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0o644); err != nil {
		t.Fatalf("write audio fixture: %v", err)
	}
	ref, err := store.Store(audioPath, media.MediaMeta{
		Filename:      filename,
		ContentType:   contentType,
		CleanupPolicy: media.CleanupPolicyForgetOnly,
	}, "scope-voice")
	if err != nil {
		t.Fatalf("store audio fixture: %v", err)
	}
	return ref
}

func TestTranscribeAudioInMessage_UntranscribedAudioTellsTheAgentNotToImprovise(t *testing.T) {
	cases := map[string]struct {
		transcriber asr.Transcriber
		note        string
	}{
		"no transcriber":     {nil, noTranscriberAudioNote},
		"transcriber fails":  {failingTranscriber{}, failedTranscriptionAudioNote},
		"no speech in audio": {&fixedTranscriber{text: ""}, noSpeechAudioNote},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := media.NewFileMediaStore()
			ref := storeAudio(t, store, "voice.ogg", "audio/ogg")
			al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, transcriber: tc.transcriber}

			msg := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
				Content: "transcreva na íntegra [audio: voice.ogg]",
				Media:   []string{ref},
			})

			if want := "transcreva na íntegra " + tc.note; msg.Content != want {
				t.Fatalf("content = %q, want %q", msg.Content, want)
			}
			if audioPlaceholderRegex.MatchString(msg.Content) || audioAnnotationRe.MatchString(msg.Content) {
				t.Fatal("note matches an audio regex and would be overwritten by a later pass")
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
	ref := storeAudio(t, store, "musica.mp3", "audio/mpeg")
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store}

	msg := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "salva no meu Drive",
		Media:   []string{ref},
	})

	if want := "salva no meu Drive\n" + noTranscriberAudioNote; msg.Content != want {
		t.Fatalf("content = %q, want %q", msg.Content, want)
	}
	if len(msg.Media) != 1 || msg.Media[0] != ref {
		t.Fatalf("media = %v, want the attachment kept", msg.Media)
	}
}

// The web channel (gRPC) and the pod /webhook route hand the agent an absolute
// workspace path, not a media:// ref. The incident behind the note was a web upload.
func TestTranscribeAudioInMessage_AbsolutePathAudioGetsNote(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "uploads", "a071539d-e2e-audio-teste.ogg")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("fake audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore()}

	msg := al.prepareInboundMessageForAgent(context.Background(), bus.InboundMessage{
		Context: bus.InboundContext{Channel: "grpc", ChatID: "run-1", ChatType: "direct", SenderID: "web"},
		Content: "transcreva na íntegra o áudio anexado\n[ANEXOS]\n- uploads/a071539d-e2e-audio-teste.ogg",
		Media:   []string{abs},
	})

	if !strings.HasSuffix(msg.Content, "\n"+noTranscriberAudioNote) {
		t.Fatalf("no note for absolute-path audio; content = %q", msg.Content)
	}
	if len(msg.Media) != 1 || msg.Media[0] != abs {
		t.Fatalf("media = %v, want the upload kept", msg.Media)
	}
}

// The agent sends the placeholder only when the channel deferred it, i.e. when
// the content carries an audio annotation (pkg/channels/base.go).
func TestPrepareInboundMessage_SendsPlaceholderOnlyWhenChannelDeferredIt(t *testing.T) {
	cases := map[string]struct {
		content string
		want    int
	}{
		"voice annotation, deferred":           {"[voice]", 1},
		"audio sent as document, not deferred": {"salva no meu Drive\n[file]", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := media.NewFileMediaStore()
			ref := storeAudio(t, store, "musica.mp3", "audio/mpeg")
			cm := &countingChannelManager{}
			al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, channelManager: cm}

			al.prepareInboundMessageForAgent(context.Background(), bus.InboundMessage{
				Context: bus.InboundContext{Channel: "telegram", ChatID: "1", ChatType: "direct", SenderID: "u"},
				Content: tc.content,
				Media:   []string{ref},
			})

			if cm.placeholders != tc.want {
				t.Fatalf("agent sent %d placeholders, want %d", cm.placeholders, tc.want)
			}
		})
	}
}

// Without a transcriber there was no transcription, so no "No voice detected"
// feedback either, even with EchoTranscription on.
func TestTranscribeAudioInMessage_NoFeedbackWithoutTranscriber(t *testing.T) {
	store := media.NewFileMediaStore()
	ref := storeAudio(t, store, "voice.ogg", "audio/ogg")
	cm := &countingChannelManager{}
	cfg := &config.Config{}
	cfg.Voice.EchoTranscription = true
	al := &AgentLoop{cfg: cfg, mediaStore: store, channelManager: cm}

	al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{Content: "[voice]", Media: []string{ref}})

	if cm.messages != 0 {
		t.Fatalf("sent %d feedback messages, want 0", cm.messages)
	}
}

func TestTranscribeAudioInMessage_TranscribedAudioBecomesVoiceText(t *testing.T) {
	store := media.NewFileMediaStore()
	ref := storeAudio(t, store, "voice.ogg", "audio/ogg")
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, transcriber: &fixedTranscriber{text: "olá"}}

	msg := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "[voice]",
		Media:   []string{ref},
	})

	if msg.Content != "[voice: olá]" || len(msg.Media) != 0 {
		t.Fatalf("got content %q media %v", msg.Content, msg.Media)
	}
}
