package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
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
// audio transcribed, or the note that replaces it. The message the caller holds
// stays as sent, which is what the screen places.
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
			sent := providers.Message{Role: "user", Content: "[voice]", Media: []string{audio}, TranscribeAudio: true}

			got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

			if got.Content != tc.content {
				t.Errorf("content = %q, want %q", got.Content, tc.content)
			}
			if kept := len(got.Media) == 1; kept != tc.keepsAudio {
				t.Errorf("media = %v, keeps audio = %v, want %v", got.Media, kept, tc.keepsAudio)
			}
			if got.TranscribeAudio {
				t.Error("the mark stayed on: a later read would transcribe it again")
			}
			if sent.Content != "[voice]" || len(sent.Media) != 1 || sent.Media[0] != audio || !sent.TranscribeAudio {
				t.Errorf("the message as sent changed: %+v", sent)
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
	sent := providers.Message{Role: "user", Content: "fala\n" + failedTranscriptionAudioNote, Media: []string{audio}}

	got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

	if tr.calls() != 0 {
		t.Errorf("transcribed %d times an unmarked message", tr.calls())
	}
	if got.Content != sent.Content || len(got.Media) != 1 {
		t.Errorf("message changed: %+v", got)
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
	sent := providers.Message{Role: "user", Content: "olha isso", Media: []string{photo}, TranscribeAudio: true}

	got := al.transcribeJoinedAudio(t.Context(), joinedTurn(), sent)

	if tr.calls() != 0 || got.Content != "olha isso" || len(got.Media) != 1 || got.Media[0] != photo {
		t.Errorf("an image changed or was transcribed: %+v (%d calls)", got, tr.calls())
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
		t.Fatalf(
			"marks = %v, want text unmarked and file marked",
			[]bool{queued[0].TranscribeAudio, queued[1].TranscribeAudio},
		)
	}
}

// newAudioSteeringLoop is newSessionSteeringLoop that also hands back the bus:
// al.bus is an interface without OutboundChan.
func newAudioSteeringLoop(t *testing.T, provider providers.LLMProvider) (*AgentLoop, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: t.TempDir(), ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 10,
	}}}
	msgBus := bus.NewMessageBus()
	return NewAgentLoop(cfg, msgBus, provider), msgBus
}

func runDirectOn(ctx context.Context, al *AgentLoop, content, sessionKey, channel, chatID string) <-chan directResult {
	done := make(chan directResult, 1)
	go func() {
		resp, err := al.ProcessDirectWithChannel(ctx, content, sessionKey, channel, chatID)
		done <- directResult{resp: resp, err: err}
	}()
	return done
}

// collectSteeringReads keeps the content of each steering_read the loop
// publishes; read stops the collector and returns them.
func collectSteeringReads(t *testing.T, msgBus *bus.MessageBus) (read func() []string) {
	t.Helper()
	var reads []string
	take := func(out bus.OutboundMessage) {
		if out.Context.Raw[metadataKeyMessageKind] == messageKindSteeringRead {
			reads = append(reads, out.Content)
		}
	}
	ctx, stop := context.WithCancel(t.Context())
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for {
			select {
			case out := <-msgBus.OutboundChan():
				take(out)
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() []string {
		stop()
		<-collected
		for {
			select {
			case out := <-msgBus.OutboundChan():
				take(out)
			default:
				return reads
			}
		}
	}
}

// An audio sent while the conversation answers reaches the model and the
// history transcribed; the screen is told the message as it was sent, which is
// what ethos matches the send by (rpcchannel.go, joined.take).
func TestSteerSession_AudioReachesTheTurnTranscribed(t *testing.T) {
	provider := newSessionRecordingProvider("stale answer", "answer that heard the audio")
	al, msgBus := newAudioSteeringLoop(t, provider)
	t.Cleanup(provider.unblock)
	al.SetMediaStore(media.NewFileMediaStore())
	al.SetTranscriber(&fixedTranscriber{text: "manda o resumo por e-mail"})
	reads := collectSteeringReads(t, msgBus)

	done := runDirectOn(t.Context(), al, "build the deck", conversationA, "grpc", "run-a")
	waitStarted(t, provider.started, done)
	if err := al.SteerSession(conversationA, "[voice]", []string{writeJoinedAudio(t)}); err != nil {
		t.Fatalf("SteerSession: %v", err)
	}
	provider.unblock()
	waitDirect(t, done)

	calls := provider.calls()
	if len(calls) != 2 || !sentContains(calls[1], "[voice: manda o resumo por e-mail]") {
		t.Fatalf("got %d calls; the second must carry the transcription", len(calls))
	}
	history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(conversationA)
	if n := occurrences(history, "[voice: manda o resumo por e-mail]"); n != 1 {
		t.Errorf("the history holds the transcription %d times, want once", n)
	}
	if got := reads(); !slices.Equal(got, []string{"[voice]"}) {
		t.Errorf("steering_read = %q, want the message as sent", got)
	}
}

// A message no turn read before its turn ended runs in the continuation that
// ethos starts for it (drainJoined): it is transcribed there too.
func TestContinueDirect_TranscribesAudioQueuedBySteerSession(t *testing.T) {
	provider := newSessionRecordingProvider("answer that heard the audio", "")
	provider.unblock()
	al := newSessionSteeringLoop(t, provider)
	al.SetMediaStore(media.NewFileMediaStore())
	al.SetTranscriber(&fixedTranscriber{text: "manda o resumo"})
	if err := al.SteerSession(conversationA, "[voice]", []string{writeJoinedAudio(t)}); err != nil {
		t.Fatal(err)
	}

	result, ran, err := al.ContinueDirect(
		t.Context(),
		DirectContinuation{SessionKey: conversationA, Channel: "test", ChatID: "run-a"},
	)

	if err != nil || !ran {
		t.Fatalf("ContinueDirect = ran %v, err %v", ran, err)
	}
	if result.Content != "answer that heard the audio" {
		t.Errorf("answered %q", result.Content)
	}
	if calls := provider.calls(); len(calls) != 1 || !sentContains(calls[0], "[voice: manda o resumo]") {
		t.Fatalf("got %d calls; the continuation must carry the transcription", len(calls))
	}
}

// blockingTranscriber holds the transcription until its context ends, as a
// slow speech-to-text call does.
type blockingTranscriber struct {
	started chan struct{}
	err     chan error
}

func (b *blockingTranscriber) Name() string { return "blocking" }

func (b *blockingTranscriber) Transcribe(ctx context.Context, _ string) (*asr.TranscriptionResponse, error) {
	close(b.started)
	<-ctx.Done()
	b.err <- ctx.Err()
	return nil, ctx.Err()
}

// A stop during the transcription cuts it with the turn: it runs on the
// turn's context, not on one of its own. The audio did not fail, so the
// history keeps the note that says the reply was interrupted, not the
// failure note. (MST-553)
func TestSteerSession_StopCutsTheTranscription(t *testing.T) {
	provider := newSessionRecordingProvider("stale answer", "later")
	al := newSessionSteeringLoop(t, provider)
	t.Cleanup(provider.unblock)
	al.SetMediaStore(media.NewFileMediaStore())
	tr := &blockingTranscriber{started: make(chan struct{}), err: make(chan error, 1)}
	al.SetTranscriber(tr)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()

	done := runDirectOn(ctx, al, "build the deck", conversationA, "test", "run-a")
	waitStarted(t, provider.started, done)
	if err := al.SteerSession(conversationA, "[voice]", []string{writeJoinedAudio(t)}); err != nil {
		t.Fatal(err)
	}
	provider.unblock()
	select {
	case <-tr.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never transcribed the audio")
	}
	stop()

	select {
	case err := <-tr.err:
		if err == nil {
			t.Error("the transcription ended without its context ending")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not reach the transcription")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not end after the stop")
	}
	history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(conversationA)
	if n := occurrences(history, interruptedTranscriptionAudioNote); n != 1 {
		t.Errorf("the history holds the interrupted note %d times, want once", n)
	}
	if occurrences(history, failedTranscriptionAudioNote) != 0 {
		t.Error("the history calls a transcription the stop cut a failure")
	}
}

// taskTranscriber answers each call with the next of texts and keeps the task
// each call went out on: the gateway groups a transcription with the calls of
// that task.
type taskTranscriber struct {
	mu    sync.Mutex
	texts []string
	tasks []string
}

func (tr *taskTranscriber) Name() string { return "task" }

func (tr *taskTranscriber) Transcribe(ctx context.Context, _ string) (*asr.TranscriptionResponse, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.tasks = append(tr.tasks, protocoltypes.TaskIDFromContext(ctx))
	return &asr.TranscriptionResponse{Text: tr.texts[(len(tr.tasks)-1)%len(tr.texts)]}, nil
}

func (tr *taskTranscriber) taskIDs() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.tasks)
}

// taskSessionProvider is sessionRecordingProvider that also keeps the task
// each call went out on.
type taskSessionProvider struct {
	*sessionRecordingProvider
	mu    sync.Mutex
	tasks []string
}

func (p *taskSessionProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.tasks = append(p.tasks, protocoltypes.TaskIDFromContext(ctx))
	p.mu.Unlock()
	return p.sessionRecordingProvider.Chat(ctx, messages, tools, model, opts)
}

func (p *taskSessionProvider) taskIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.tasks)
}

// joinAudiosMidTurn runs a turn on the web channel and, while its first call
// works, sends one audio per transcription in tr. It returns the task of each
// model call and the steering_read of each message.
func joinAudiosMidTurn(t *testing.T, mode SteeringMode, tr *taskTranscriber, audios int) (
	provider *taskSessionProvider, reads []string,
) {
	t.Helper()
	provider = &taskSessionProvider{
		sessionRecordingProvider: newSessionRecordingProvider("stale answer", "answer that heard the audio"),
	}
	al, msgBus := newAudioSteeringLoop(t, provider)
	t.Cleanup(provider.unblock)
	al.SetSteeringMode(mode)
	al.SetMediaStore(media.NewFileMediaStore())
	al.SetTranscriber(tr)
	read := collectSteeringReads(t, msgBus)

	done := runDirectOn(t.Context(), al, "build the deck", conversationA, "grpc", "run-a")
	waitStarted(t, provider.started, done)
	for range audios {
		if err := al.SteerSession(conversationA, "[voice]", []string{writeJoinedAudio(t)}); err != nil {
			t.Fatalf("SteerSession: %v", err)
		}
	}
	provider.unblock()
	waitDirect(t, done)
	return provider, read()
}

// A joined audio is a new request: its transcription goes out on the task of
// the call that answers it, not on the task of the request it joined.
func TestSteerSession_AudioIsTranscribedInTheTaskThatAnswersIt(t *testing.T) {
	tr := &taskTranscriber{texts: []string{"manda o resumo"}}

	provider, _ := joinAudiosMidTurn(t, SteeringOneAtATime, tr, 1)

	calls, heard := provider.taskIDs(), tr.taskIDs()
	if len(calls) != 2 || len(heard) != 1 {
		t.Fatalf("got %d model calls and %d transcriptions, want 2 and 1", len(calls), len(heard))
	}
	if calls[0] == "" || calls[1] == calls[0] {
		t.Fatalf("model call tasks = %q, want the answer on a task of its own", calls)
	}
	if heard[0] != calls[1] {
		t.Errorf("transcription task = %q, want the answer's %q (the request it joined is %q)",
			heard[0], calls[1], calls[0])
	}
}

// Audios read together are each a request of their own: each transcription
// goes out on its own task, none on the task of the request they joined, and
// the last is the task of the call that answers. The screen still places each
// message as it was sent.
func TestSteerSession_EachQueuedAudioIsTranscribedInItsOwnTask(t *testing.T) {
	tr := &taskTranscriber{texts: []string{"primeiro", "segundo"}}

	provider, reads := joinAudiosMidTurn(t, SteeringAll, tr, 2)

	calls, heard := provider.taskIDs(), tr.taskIDs()
	if len(calls) != 2 || len(heard) != 2 {
		t.Fatalf("got %d model calls and %d transcriptions, want 2 and 2", len(calls), len(heard))
	}
	if heard[0] == "" || heard[0] == heard[1] || slices.Contains(heard, calls[0]) {
		t.Errorf("transcription tasks = %q, want one each, none the earlier request's %q", heard, calls[0])
	}
	if heard[1] != calls[1] {
		t.Errorf("last transcription task = %q, want the answer's %q", heard[1], calls[1])
	}
	answer := provider.calls()[1]
	first := slices.IndexFunc(answer, func(m providers.Message) bool { return m.Content == "[voice: primeiro]" })
	second := slices.IndexFunc(answer, func(m providers.Message) bool { return m.Content == "[voice: segundo]" })
	if first < 0 || second < first {
		t.Errorf("transcriptions at %d and %d of the answer's messages, want each in its message, in order",
			first, second)
	}
	if !slices.Equal(reads, []string{"[voice]", "[voice]"}) {
		t.Errorf("steering_read = %q, want each message as sent", reads)
	}
}
