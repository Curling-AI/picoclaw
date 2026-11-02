package providers

import (
	"context"
	"testing"
)

func TestHistoryCallAcceptsOnlyProducerRequestIDs(t *testing.T) {
	const call = "pc-00112233445566778899aabb"
	ctx := WithHistoryCall(context.Background(), call)
	if HistoryCall(ctx) != call || HistoryCall(nil) != "" {
		t.Fatal("producer request identity did not survive context propagation")
	}
	for _, invalid := range []string{"", "pc-no", "pc-00112233445566778899aabz", "00112233445566778899aabb"} {
		if ValidHistoryCallID(invalid) || HistoryCall(WithHistoryCall(context.Background(), invalid)) != "" {
			t.Fatalf("accepted invalid request identity %q", invalid)
		}
	}
}
