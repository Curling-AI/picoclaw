package channels

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

func TestSeenMessage_FirstAcceptThenDuplicate(t *testing.T) {
	ch := NewBaseChannel("test", nil, nil, nil)

	if ch.seenMessage("1700000000.000100") {
		t.Fatal("first delivery must not be treated as duplicate")
	}
	if !ch.seenMessage("1700000000.000100") {
		t.Fatal("immediate redelivery must be treated as duplicate")
	}
	if ch.seenMessage("1700000000.000200") {
		t.Fatal("a different MessageID must not be treated as duplicate")
	}
}

func TestSeenMessage_EmptyIDNeverDeduped(t *testing.T) {
	ch := NewBaseChannel("test", nil, nil, nil)

	for i := 0; i < 3; i++ {
		if ch.seenMessage("") {
			t.Fatalf("call %d: empty MessageID must never be deduplicated", i)
		}
	}
}

func TestSeenMessage_ExpiresAfterWindow(t *testing.T) {
	ch := NewBaseChannel("test", nil, nil, nil)
	const id = "expiring"

	if ch.seenMessage(id) {
		t.Fatal("first delivery must not be treated as duplicate")
	}

	ch.dedupMu.Lock()
	ch.dedupSeen[id] = time.Now().Add(-dedupWindow - time.Second)
	ch.dedupMu.Unlock()

	if ch.seenMessage(id) {
		t.Fatal("MessageID older than dedupWindow must be accepted again")
	}
	if !ch.seenMessage(id) {
		t.Fatal("refreshed MessageID must be deduplicated again")
	}
}

func TestSeenMessage_PruneKeepsMapBounded(t *testing.T) {
	ch := NewBaseChannel("test", nil, nil, nil)

	const total = dedupMaxEntries*2 + 123
	for i := 0; i < total; i++ {
		if ch.seenMessage(fmt.Sprintf("burst-%d", i)) {
			t.Fatalf("id %d: distinct MessageID must not be treated as duplicate", i)
		}
		ch.dedupMu.Lock()
		size := len(ch.dedupSeen)
		ch.dedupMu.Unlock()
		if size > dedupMaxEntries {
			t.Fatalf("after %d inserts map holds %d entries, cap is %d", i+1, size, dedupMaxEntries)
		}
	}

	// The most recent ID must survive the burst eviction.
	if !ch.seenMessage(fmt.Sprintf("burst-%d", total-1)) {
		t.Fatal("most recently recorded MessageID must still be deduplicated")
	}
}

func TestHandleInboundContext_DedupsSecondDelivery(t *testing.T) {
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch := NewBaseChannel("slack", nil, msgBus, nil)
	inbound := bus.InboundContext{
		ChatID:    "C123",
		SenderID:  "U1",
		MessageID: "1700000000.000100",
	}

	if err := ch.HandleInboundContext(context.Background(), inbound.ChatID, "hello @bot", nil, inbound); err != nil {
		t.Fatalf("first HandleInboundContext: %v", err)
	}
	select {
	case msg := <-msgBus.InboundChan():
		if msg.MessageID != inbound.MessageID {
			t.Fatalf("MessageID = %q, want %q", msg.MessageID, inbound.MessageID)
		}
	case <-time.After(time.Second):
		t.Fatal("first delivery was not published")
	}

	// Slack delivers the same post again as app_mention with the same ts.
	inbound.Mentioned = true
	if err := ch.HandleInboundContext(context.Background(), inbound.ChatID, "hello @bot", nil, inbound); err != nil {
		t.Fatalf("second HandleInboundContext: %v", err)
	}
	select {
	case msg := <-msgBus.InboundChan():
		t.Fatalf("duplicate delivery was published: %+v", msg.Context)
	case <-time.After(100 * time.Millisecond):
	}
}
