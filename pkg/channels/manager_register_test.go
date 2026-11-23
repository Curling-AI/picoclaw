package channels

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// A channel registered over one that already started takes over the
// worker's outbound only when it asks to (OutboundTakeover); any other keeps
// today's behavior, where the worker sends through the channel it started
// with. (seucaranguejo fork — MST-1021)

// takeoverChannel asks to take over the outbound when takeOver is true.
type takeoverChannel struct {
	mockMediaChannel
	takeOver bool
}

func (c *takeoverChannel) TakesOverOutbound() bool { return c.takeOver }

// takeoverChannelWithLength takes over the outbound and splits at maxLen.
type takeoverChannelWithLength struct {
	mockChannelWithLength
}

func (*takeoverChannelWithLength) TakesOverOutbound() bool { return true }

func startedWorker(t *testing.T, m *Manager, name string, ch Channel) *channelWorker {
	t.Helper()
	w := newChannelWorker(name, ch, "test")
	m.channels[name] = ch
	m.workers[name] = w
	go m.runWorker(t.Context(), name, w)
	go m.runMediaWorker(t.Context(), name, w)
	return w
}

func sendTextAndMedia(t *testing.T, m *Manager) {
	t.Helper()
	err := m.SendMessage(context.Background(), testOutboundMessage(bus.OutboundMessage{
		Channel: "slack", ChatID: "C1", Content: "hi",
	}))
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	err = m.SendMedia(context.Background(), testOutboundMediaMessage(bus.OutboundMediaMessage{
		Channel: "slack", ChatID: "C1", Parts: []bus.MediaPart{{Ref: "media://abc"}},
	}))
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
}

func TestRegisterChannelThatTakesOverRoutesSendsToIt(t *testing.T) {
	m := newTestManager()
	started := &mockMediaChannel{}
	startedWorker(t, m, "slack", started)
	registered := &takeoverChannel{takeOver: true}

	m.RegisterChannel("slack", registered)
	sendTextAndMedia(t, m)

	if len(registered.sentMessages) != 1 || len(registered.sentMediaMessages) != 1 {
		t.Fatalf("registered channel got %d messages and %d media, want 1 and 1",
			len(registered.sentMessages), len(registered.sentMediaMessages))
	}
	if len(started.sentMessages) != 0 || len(started.sentMediaMessages) != 0 {
		t.Fatalf("started channel got %d messages and %d media, want none",
			len(started.sentMessages), len(started.sentMediaMessages))
	}
}

// A wrapper that does not ask (the gateway's Telegram media wrapper) leaves
// the worker on the channel it started with, as before; so does one that
// answers no.
func TestRegisterChannelThatDoesNotTakeOverLeavesTheWorker(t *testing.T) {
	for name, registered := range map[string]interface {
		Channel
		sent() (int, int)
	}{
		"plain wrapper": &plainWrapper{},
		"answers no":    &takeoverReport{takeoverChannel{takeOver: false}},
	} {
		t.Run(name, func(t *testing.T) {
			m := newTestManager()
			started := &mockMediaChannel{}
			startedWorker(t, m, "slack", started)

			m.RegisterChannel("slack", registered)
			sendTextAndMedia(t, m)

			if len(started.sentMessages) != 1 || len(started.sentMediaMessages) != 1 {
				t.Fatalf("started channel got %d messages and %d media, want 1 and 1",
					len(started.sentMessages), len(started.sentMediaMessages))
			}
			if messages, media := registered.sent(); messages != 0 || media != 0 {
				t.Fatalf("registered channel got %d messages and %d media, want none", messages, media)
			}
		})
	}
}

type plainWrapper struct{ mockMediaChannel }

func (w *plainWrapper) sent() (int, int) { return len(w.sentMessages), len(w.sentMediaMessages) }

type takeoverReport struct{ takeoverChannel }

func (w *takeoverReport) sent() (int, int) { return len(w.sentMessages), len(w.sentMediaMessages) }

// The queued path (what the bus publishes) goes to the channel that took over,
// split by that channel's length.
func TestRegisterChannelThatTakesOverGetsQueuedMessages(t *testing.T) {
	m := newTestManager()
	leaked := make(chan string, 4)
	started := &mockChannel{sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
		leaked <- msg.Content
		return nil
	}}
	w := startedWorker(t, m, "slack", started)
	received := make(chan string, 4)
	m.RegisterChannel("slack", &takeoverChannelWithLength{mockChannelWithLength{
		mockChannel: mockChannel{sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
			received <- msg.Content
			return nil
		}},
		maxLen: 5,
	}})

	w.queue <- testOutboundMessage(bus.OutboundMessage{Channel: "slack", ChatID: "C1", Content: "hello world"})

	var chunks []string
	for len(chunks) < 2 {
		select {
		case chunk := <-received:
			chunks = append(chunks, chunk)
		case chunk := <-leaked:
			t.Fatalf("started channel got %q", chunk)
		case <-time.After(2 * time.Second):
			t.Fatalf("registered channel got %v, want the message in chunks of 5", chunks)
		}
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) > 5 {
			t.Fatalf("chunk %q is longer than the registered channel's 5", chunk)
		}
	}
}

func TestRegisterChannelThatTakesOverAfterReload(t *testing.T) {
	const channelType = "mst1021-reload-test"
	reloaded := &mockMediaChannel{}
	RegisterFactory(channelType, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
		return reloaded, nil
	})
	old := config.DefaultConfig()
	m := &Manager{
		channels:      map[string]Channel{},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        old,
		channelHashes: toChannelHashes(old),
	}
	next := config.DefaultConfig()
	next.Channels["slack"] = &config.Channel{Enabled: true, Type: channelType, Settings: config.RawNode(`{"v":1}`)}
	if err := m.Reload(t.Context(), next); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	if _, ok := m.workers["slack"]; !ok {
		t.Fatal("Reload started no worker for slack")
	}
	registered := &takeoverChannel{takeOver: true}

	m.RegisterChannel("slack", registered)
	sendTextAndMedia(t, m)

	if len(registered.sentMessages) != 1 || len(registered.sentMediaMessages) != 1 {
		t.Fatalf("registered got %d messages and %d media, want 1 and 1",
			len(registered.sentMessages), len(registered.sentMediaMessages))
	}
	if len(reloaded.sentMessages) != 0 || len(reloaded.sentMediaMessages) != 0 {
		t.Fatalf("reloaded got %d messages and %d media, want none",
			len(reloaded.sentMessages), len(reloaded.sentMediaMessages))
	}
}

// Unregistering still drains and drops the worker; registering afterwards
// brings none back.
func TestRegisterChannelAfterUnregisterStartsNoWorker(t *testing.T) {
	m := newTestManager()
	startedWorker(t, m, "slack", &mockChannel{})
	m.UnregisterChannel("slack")
	next := &takeoverChannel{takeOver: true}

	m.RegisterChannel("slack", next)

	if _, ok := m.workers["slack"]; ok {
		t.Fatal("a worker came back for slack")
	}
	err := m.SendMessage(context.Background(), testOutboundMessage(bus.OutboundMessage{
		Channel: "slack", ChatID: "C1", Content: "hi",
	}))
	if err == nil || !strings.Contains(err.Error(), "has no active worker") {
		t.Fatalf("SendMessage() error = %v, want no active worker", err)
	}
	if len(next.sentMessages) != 0 {
		t.Fatalf("next channel got %d messages, want none", len(next.sentMessages))
	}
}

// The worker reads the channel while RegisterChannel swaps it (-race catches an
// unsynchronized swap). Without -race it still checks the routing: no send is
// lost, and once the swaps are done the next send goes to the last channel
// registered, not to the one the worker started with.
func TestRegisterChannelWhileTheWorkerSends(t *testing.T) {
	m := newTestManager()
	first := &mockChannel{}
	m.channels["slack"] = first
	m.workers["slack"] = &channelWorker{ch: first, limiter: rate.NewLimiter(rate.Inf, 1)}

	const rounds = 50
	registered := make([]*takeoverChannel, 0, rounds)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range rounds {
			ch := &takeoverChannel{takeOver: true}
			registered = append(registered, ch)
			m.RegisterChannel("slack", ch)
		}
	}()
	go func() {
		defer wg.Done()
		for range rounds {
			_ = m.SendMessage(context.Background(), testOutboundMessage(bus.OutboundMessage{
				Channel: "slack", ChatID: "C1", Content: "hi",
			}))
		}
	}()
	wg.Wait()

	total := len(first.sentMessages)
	for _, ch := range registered {
		total += len(ch.sentMessages)
	}
	if total != rounds {
		t.Fatalf("channels got %d messages in total, want %d", total, rounds)
	}

	last := registered[len(registered)-1]
	before, firstBefore := len(last.sentMessages), len(first.sentMessages)
	err := m.SendMessage(context.Background(), testOutboundMessage(bus.OutboundMessage{
		Channel: "slack", ChatID: "C1", Content: "after",
	}))
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if len(last.sentMessages) != before+1 {
		t.Fatalf("last registered channel got %d messages after the swaps, want %d",
			len(last.sentMessages), before+1)
	}
	if len(first.sentMessages) != firstBefore {
		t.Fatal("the channel the worker started with got the message sent after the swaps")
	}
}

// SendMessage (the message tool's path) splits by the length of the channel
// that took over, and sends to it.
func TestRegisterChannelThatTakesOverSendMessageSplitsByItsLength(t *testing.T) {
	m := newTestManager()
	started := &mockChannel{}
	startedWorker(t, m, "slack", started)
	var received []string
	m.RegisterChannel("slack", &takeoverChannelWithLength{mockChannelWithLength{
		mockChannel: mockChannel{sendFn: func(_ context.Context, msg bus.OutboundMessage) error {
			received = append(received, msg.Content)
			return nil
		}},
		maxLen: 5,
	}})

	err := m.SendMessage(context.Background(), testOutboundMessage(bus.OutboundMessage{
		Channel: "slack", ChatID: "C1", Content: "hello world",
	}))
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}

	if len(received) < 2 || len(started.sentMessages) != 0 {
		t.Fatalf("registered got %v and started %d, want chunks of 5 only on the registered one",
			received, len(started.sentMessages))
	}
	for _, chunk := range received {
		if len([]rune(chunk)) > 5 {
			t.Fatalf("chunk %q is longer than the registered channel's 5", chunk)
		}
	}
}
