package agent

import (
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func enabledLoopCfg() config.LoopDetectionConfig {
	return config.LoopDetectionConfig{
		Enabled:                       true,
		HistorySize:                   30,
		WarningThreshold:              4,
		CriticalThreshold:             8,
		GlobalCircuitBreakerThreshold: 12,
		Detectors: config.LoopDetectorsConfig{
			GenericRepeat: true,
		},
	}
}

func TestNewLoopDetector_DisabledReturnsNil(t *testing.T) {
	if ld := NewLoopDetector(config.LoopDetectionConfig{Enabled: false}); ld != nil {
		t.Fatal("disabled config should yield a nil detector")
	}
	// nil receiver must be safe and inert.
	var nilLD *LoopDetector
	if sig := nilLD.Record("exec", nil, "out"); sig != LoopSignalNone {
		t.Fatalf("nil detector should return None, got %v", sig)
	}
}

func TestLoopDetector_GenericRepeatEscalates(t *testing.T) {
	ld := NewLoopDetector(enabledLoopCfg())
	args := map[string]any{"cmd": "ls"}

	var lastWarn, lastBlock, lastAbort LoopSignal
	for i := 1; i <= 12; i++ {
		sig := ld.Record("exec", args, "same-output")
		switch {
		case i < 4:
			if sig != LoopSignalNone {
				t.Fatalf("call %d: expected None, got %v", i, sig)
			}
		case i >= 4 && i < 8:
			lastWarn = sig
		case i >= 8 && i < 12:
			lastBlock = sig
		case i >= 12:
			lastAbort = sig
		}
	}
	if lastWarn != LoopSignalWarning {
		t.Errorf("warning band: got %v", lastWarn)
	}
	if lastBlock != LoopSignalBlock {
		t.Errorf("block band: got %v", lastBlock)
	}
	if lastAbort != LoopSignalAbort {
		t.Errorf("abort band: got %v", lastAbort)
	}
}

func TestLoopDetector_DifferentArgsDoNotTrip(t *testing.T) {
	ld := NewLoopDetector(enabledLoopCfg())
	for i := 0; i < 20; i++ {
		// Unique args each call → generic-repeat must never fire.
		sig := ld.Record("exec", map[string]any{"n": i}, "out")
		if sig == LoopSignalBlock || sig == LoopSignalAbort {
			t.Fatalf("unique-arg calls tripped at i=%d: %v", i, sig)
		}
	}
}

func TestLoopDetector_ToolFrequencyFiresOnVaryingArgs(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Detectors = config.LoopDetectorsConfig{ToolFrequency: true}
	ld := NewLoopDetector(cfg)
	// abortAt = 12 + 6 = 18 calls regardless of args.
	var sig LoopSignal
	for i := 0; i < 18; i++ {
		sig = ld.Record("web_search", map[string]any{"q": i}, "out")
	}
	if sig != LoopSignalAbort {
		t.Fatalf("tool frequency should abort at 18 calls, got %v", sig)
	}
}

func TestLoopDetector_ArgumentDriftFiresOnGrowingArgs(t *testing.T) {
	cfg := enabledLoopCfg()
	cfg.Detectors = config.LoopDetectorsConfig{ArgumentDrift: true}
	ld := NewLoopDetector(cfg)
	q := ""
	var sawBlock bool
	for i := 0; i < 12; i++ {
		q += "more " // monotonically growing args
		if ld.Record("web_search", map[string]any{"q": q}, "out") == LoopSignalBlock {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatal("argument drift should escalate to Block on growing args")
	}
}

func TestNormalizeArgs_DeterministicRegardlessOfKeyOrder(t *testing.T) {
	a := normalizeArgs(map[string]any{"a": 1, "b": 2})
	b := normalizeArgs(map[string]any{"b": 2, "a": 1})
	if a != b {
		t.Fatalf("normalizeArgs not order-independent: %q vs %q", a, b)
	}
	if normalizeArgs(nil) != "{}" {
		t.Errorf("empty args should be {}, got %q", normalizeArgs(nil))
	}
}

func TestShouldSuppressToolErrorForUser(t *testing.T) {
	if shouldSuppressToolErrorForUser("read_file", false) {
		t.Error("must not suppress when disabled")
	}
	if !shouldSuppressToolErrorForUser("read_file", true) {
		t.Error("read_file is non-mutating; should suppress when enabled")
	}
	if shouldSuppressToolErrorForUser("exec", true) {
		t.Error("exec is mutating; must never suppress")
	}
}

func TestBuildLoopAbortSummary(t *testing.T) {
	log := []ToolExecutionRecord{
		{Name: "web_search", Success: true},
		{Name: "web_search", Success: false},
		{Name: "read_file", Success: true},
	}
	s := buildLoopAbortSummary(log, "circuit breaker")
	if !strings.Contains(s, "3 tool calls") {
		t.Errorf("summary missing call count: %q", s)
	}
	if !strings.Contains(s, "circuit breaker") {
		t.Errorf("summary missing pattern: %q", s)
	}
	if !strings.Contains(s, "web_search") {
		t.Errorf("summary missing last tools: %q", s)
	}
}
