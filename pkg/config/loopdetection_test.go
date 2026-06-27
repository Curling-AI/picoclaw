package config

import (
	"encoding/json"
	"testing"
)

// The product (seucaranguejo) serializes these keys into the gateway config.
// They must deserialize into the typed structs verbatim, or the feature is inert.
func TestLoopDetectionConfig_DeserializesProductKeys(t *testing.T) {
	raw := `{
		"tools": {"loop_detection": {
			"enabled": true,
			"history_size": 30,
			"warning_threshold": 10,
			"critical_threshold": 20
		}},
		"messages": {"suppress_tool_errors": true}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ld := cfg.Tools.LoopDetection
	if !ld.Enabled || ld.HistorySize != 30 || ld.WarningThreshold != 10 || ld.CriticalThreshold != 20 {
		t.Fatalf("loop_detection not parsed: %+v", ld)
	}
	if !cfg.Messages.SuppressToolErrors {
		t.Fatalf("messages.suppress_tool_errors not parsed: %+v", cfg.Messages)
	}
}

func TestLoopDetectionConfig_Validate(t *testing.T) {
	// Disabled config never errors regardless of thresholds.
	if err := (&LoopDetectionConfig{Enabled: false}).Validate(); err != nil {
		t.Errorf("disabled config should validate: %v", err)
	}
	// Strictly increasing thresholds are required when enabled.
	ok := &LoopDetectionConfig{Enabled: true, WarningThreshold: 10, CriticalThreshold: 20, GlobalCircuitBreakerThreshold: 30}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	bad := []LoopDetectionConfig{
		{Enabled: true, WarningThreshold: 0, CriticalThreshold: 20, GlobalCircuitBreakerThreshold: 30},
		{Enabled: true, WarningThreshold: 20, CriticalThreshold: 20, GlobalCircuitBreakerThreshold: 30},
		{Enabled: true, WarningThreshold: 10, CriticalThreshold: 30, GlobalCircuitBreakerThreshold: 30},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("bad config %d should have failed: %+v", i, c)
		}
	}
}

// The product serializes these summarization keys into agents.defaults; they
// must deserialize into the typed fields verbatim (upstream uses different keys).
func TestSummarizationConfig_DeserializesProductKeys(t *testing.T) {
	raw := `{"agents":{"defaults":{
		"max_history_messages": 50,
		"summarization_threshold_percent": 80,
		"keep_last_messages": 8
	}}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := cfg.Agents.Defaults
	if d.MaxHistoryMessages != 50 || d.SummarizationThresholdPercent != 80 || d.KeepLastMessages != 8 {
		t.Fatalf("summarization keys not parsed: %+v", d)
	}
}

func TestDefaultConfig_SummarizationDefaults(t *testing.T) {
	d := DefaultConfig().Agents.Defaults
	// Fork 1c228bd8: message-count trigger disabled, 90% token threshold, keep 6.
	if d.MaxHistoryMessages != 0 {
		t.Errorf("MaxHistoryMessages default = %d, want 0 (disabled)", d.MaxHistoryMessages)
	}
	if d.SummarizationThresholdPercent != 90 {
		t.Errorf("SummarizationThresholdPercent default = %d, want 90", d.SummarizationThresholdPercent)
	}
	if d.KeepLastMessages != 6 {
		t.Errorf("KeepLastMessages default = %d, want 6", d.KeepLastMessages)
	}
}

func TestDefaultConfig_LoopDetectionDefaults(t *testing.T) {
	cfg := DefaultConfig()
	ld := cfg.Tools.LoopDetection
	if ld.Enabled {
		t.Error("loop detection must default to disabled (zero-risk rollout)")
	}
	if ld.WarningThreshold >= ld.CriticalThreshold || ld.CriticalThreshold >= ld.GlobalCircuitBreakerThreshold {
		t.Errorf("default thresholds not strictly increasing: %+v", ld)
	}
	if cfg.Agents.Defaults.TimeoutSeconds != 600 {
		t.Errorf("default TimeoutSeconds = %d, want 600", cfg.Agents.Defaults.TimeoutSeconds)
	}
}
