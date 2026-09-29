package tools

import "testing"

func newTTLTestRegistry(t *testing.T) *ToolRegistry {
	t.Helper()
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{name: "core_tool", desc: "core"})
	r.RegisterHidden(&mockRegistryTool{name: "mcp_a", desc: "hidden a"})
	r.RegisterHidden(&mockRegistryTool{name: "mcp_b", desc: "hidden b"})
	return r
}

func TestEnsureVisible_RevivesExpiredHiddenTools(t *testing.T) {
	r := newTTLTestRegistry(t)

	revived := r.EnsureVisible([]string{"mcp_a", "core_tool", "unknown"}, 20)
	if len(revived) != 1 || revived[0] != "mcp_a" {
		t.Fatalf("revived = %v, want [mcp_a] (core and unknown ignored)", revived)
	}
	if _, ok := r.Get("mcp_a"); !ok {
		t.Fatal("mcp_a should be callable after EnsureVisible")
	}
	if _, ok := r.Get("mcp_b"); ok {
		t.Fatal("mcp_b was not referenced and must stay hidden")
	}

	// Already-visible tools are not "revived" again (idempotent, no log spam).
	if again := r.EnsureVisible([]string{"mcp_a"}, 20); len(again) != 0 {
		t.Fatalf("second EnsureVisible revived %v, want none", again)
	}
}

func TestTouchTools_SlidingTTLKeepsUsedToolsAlive(t *testing.T) {
	r := newTTLTestRegistry(t)
	r.PromoteTools([]string{"mcp_a", "mcp_b"}, 2)

	// Two rounds: mcp_a is used each round (touched), mcp_b sits idle.
	for i := 0; i < 2; i++ {
		r.TouchTools([]string{"mcp_a"}, 2)
		r.TickTTL()
	}

	if _, ok := r.Get("mcp_a"); !ok {
		t.Fatal("mcp_a was used every round and must still be callable (sliding TTL)")
	}
	if _, ok := r.Get("mcp_b"); ok {
		t.Fatal("mcp_b sat idle for its full TTL and must have expired")
	}
}

func TestTouchTools_DoesNotReviveExpired(t *testing.T) {
	r := newTTLTestRegistry(t)

	r.TouchTools([]string{"mcp_a"}, 20)
	if _, ok := r.Get("mcp_a"); ok {
		t.Fatal("TouchTools must not revive an expired tool — that is EnsureVisible's job")
	}
}

func TestReviveExpired_KeepsOrderAndStopsAtLimit(t *testing.T) {
	r := newTTLTestRegistry(t)
	r.RegisterHidden(&mockRegistryTool{name: "mcp_c", desc: "hidden c"})
	r.PromoteTools([]string{"mcp_b"}, 20)

	revived := r.ReviveExpired([]string{"mcp_b", "core_tool", "mcp_c", "mcp_a"}, 20, 1)
	if len(revived) != 1 || revived[0] != "mcp_c" {
		t.Fatalf("revived = %v, want [mcp_c] (live, core skipped; limit 1)", revived)
	}
	if _, ok := r.Get("mcp_a"); ok {
		t.Fatal("mcp_a is past the limit and must stay hidden")
	}
}
