package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
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

// The web channel (gRPC) and the pod /message route hand the agent an absolute
// workspace path, not a media:// ref. The incident behind the note was a web upload.
// Extensions outside utils.IsAudioFile's list (a WhatsApp .opus export) are
// recognized by content.
func TestTranscribeAudioInMessage_AbsolutePathAudioGetsNote(t *testing.T) {
	oggOpus := append(append([]byte("OggS\x00\x02"), make([]byte, 22)...), []byte("\x13OpusHead\x01\x01")...)
	cases := map[string][]byte{
		"a071539d-e2e-audio-teste.ogg": []byte("fake audio"),
		"PTT-20261020-WA0001.opus":     oggOpus,
		"gravacao.oga":                 oggOpus,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			abs := filepath.Join(t.TempDir(), "uploads", name)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(abs, data, 0o644); err != nil {
				t.Fatal(err)
			}
			al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore()}

			msg := al.prepareInboundMessageForAgent(context.Background(), bus.InboundMessage{
				Context: bus.InboundContext{Channel: "grpc", ChatID: "run-1", ChatType: "direct", SenderID: "web"},
				Content: "transcreva na íntegra o áudio anexado\n[ANEXOS]\n- uploads/" + name,
				Media:   []string{abs},
			})

			if !strings.HasSuffix(msg.Content, "\n"+noTranscriberAudioNote) {
				t.Fatalf("no note for absolute-path audio; content = %q", msg.Content)
			}
			if len(msg.Media) != 1 || msg.Media[0] != abs {
				t.Fatalf("media = %v, want the upload kept", msg.Media)
			}
		})
	}
}

// A Telegram document is stored without a content type; an .opus export is
// recognized by content.
func TestTranscribeAudioInMessage_StoredOpusDocumentGetsNote(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "PTT-20261020-WA0001.opus")
	oggOpus := append(append([]byte("OggS\x00\x02"), make([]byte, 22)...), []byte("\x13OpusHead\x01\x01")...)
	if err := os.WriteFile(abs, oggOpus, 0o644); err != nil {
		t.Fatal(err)
	}
	store := media.NewFileMediaStore()
	ref, err := store.Store(abs, media.MediaMeta{
		Filename:      "PTT-20261020-WA0001.opus",
		CleanupPolicy: media.CleanupPolicyForgetOnly,
	}, "scope-doc")
	if err != nil {
		t.Fatal(err)
	}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store}

	msg := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "transcreva\n[file]",
		Media:   []string{ref},
	})

	if !strings.HasSuffix(msg.Content, "\n"+noTranscriberAudioNote) {
		t.Fatalf("no note for stored .opus document; content = %q", msg.Content)
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

// silentAudioModel answers the way the transcription model answers silence: the
// marker when the prompt asks for it, otherwise the refusal that reached the
// conversation as the person's speech in the 2026-11-02 test (MST-575).
func silentAudioModel(t *testing.T) *asr.AudioModelTranscriber {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		reply := "I'm sorry, but I cannot transcribe the audio you provided."
		if bytes.Contains(body, []byte("reply with exactly NO_SPEECH")) {
			reply = "NO_SPEECH"
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": reply},
				"finish_reason": "stop",
			}},
		}); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	cfg := &config.ModelConfig{
		ModelName:      "maestro-transcribe",
		Model:          "litellm/maestro-transcribe",
		APIBase:        server.URL,
		AudioFileParts: true,
	}
	cfg.SetAPIKey("test-key")
	tr := asr.NewAudioModelTranscriber(cfg)
	if tr == nil {
		t.Fatal("NewAudioModelTranscriber() = nil")
	}
	return tr
}

func TestTranscribeAudioInMessage_SilenceBecomesTheNoSpeechNote(t *testing.T) {
	store := media.NewFileMediaStore()
	ref := storeAudio(t, store, "voice.ogg", "audio/ogg")
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: store, transcriber: silentAudioModel(t)}

	msg := al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{
		Content: "[voice]",
		Media:   []string{ref},
	})

	if msg.Content != noSpeechAudioNote {
		t.Fatalf("got content %q, want the no-speech note", msg.Content)
	}
}

// A web upload's path carries the person's file name, which says what the audio
// is about: it must not reach the pod's log when the transcription fails.
func TestTranscribeFailureLogOmitsTheFileName(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "pod.log")
	if err := logger.EnableFileLogging(logPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.DisableFileLogging)
	dir := filepath.Join(t.TempDir(), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	audio := filepath.Join(dir, "1a2b3c4d-consulta-fulana-hiv.ogg")
	if err := os.WriteFile(audio, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	al := &AgentLoop{cfg: &config.Config{}, mediaStore: media.NewFileMediaStore(), transcriber: failingTranscriber{}}

	al.transcribeAudioInMessage(context.Background(), bus.InboundMessage{Content: "[voice]", Media: []string{audio}})

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Transcription failed") {
		t.Fatalf("the failure was not logged: %q", data)
	}
	if strings.Contains(string(data), "consulta-fulana") {
		t.Fatalf("the log carries the file name: %q", data)
	}
}

func TestSetTranscriber_ConcurrentWithTurn(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	fake := &fixedTranscriber{text: "olá"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			if i%2 == 0 {
				al.SetTranscriber(fake)
			} else {
				al.SetTranscriber(nil)
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			_, _ = al.transcribeAudio(context.Background(), "voice.ogg")
		}
	}
}
