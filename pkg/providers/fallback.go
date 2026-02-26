package providers

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// LogFunc is a callback for emitting log messages from the fallback chain.
// level is "info", "warn", or "error".
type LogFunc func(level, msg string, fields map[string]any)

// RetryConfig controls per-candidate retry behavior for transient errors.
type RetryConfig struct {
	MaxRetries     int           // default 2
	InitialBackoff time.Duration // default 2s
	BackoffFactor  float64       // default 2.0
	MaxBackoff     time.Duration // default 30s
}

// DefaultRetryConfig returns the default retry configuration.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     2,
		InitialBackoff: 2 * time.Second,
		BackoffFactor:  2.0,
		MaxBackoff:     30 * time.Second,
	}
}

// FallbackOption configures a FallbackChain.
type FallbackOption func(*FallbackChain)

// WithLogFunc sets the logging callback for the fallback chain.
func WithLogFunc(fn LogFunc) FallbackOption {
	return func(fc *FallbackChain) {
		fc.logFunc = fn
	}
}

// WithRetryConfig sets the retry configuration for transient errors.
func WithRetryConfig(cfg RetryConfig) FallbackOption {
	return func(fc *FallbackChain) {
		fc.retry = cfg
	}
}

// FallbackChain orchestrates model fallback across multiple candidates.
type FallbackChain struct {
	cooldown *CooldownTracker
	logFunc  LogFunc
	retry    RetryConfig
}

// FallbackCandidate represents one model/provider to try.
type FallbackCandidate struct {
	Provider string
	Model    string
}

// FallbackResult contains the successful response and metadata about all attempts.
type FallbackResult struct {
	Response *LLMResponse
	Provider string
	Model    string
	Attempts []FallbackAttempt
}

// FallbackAttempt records one attempt in the fallback chain.
type FallbackAttempt struct {
	Provider string
	Model    string
	Error    error
	Reason   FailoverReason
	Duration time.Duration
	Skipped  bool // true if skipped due to cooldown
	Retries  int  // number of retries attempted on this candidate
}

// NewFallbackChain creates a new fallback chain with the given cooldown tracker.
func NewFallbackChain(cooldown *CooldownTracker, opts ...FallbackOption) *FallbackChain {
	fc := &FallbackChain{
		cooldown: cooldown,
		retry:    DefaultRetryConfig(),
	}
	for _, opt := range opts {
		opt(fc)
	}
	return fc
}

// log emits a log message if a LogFunc is configured.
func (fc *FallbackChain) log(level, msg string, fields map[string]any) {
	if fc.logFunc != nil {
		fc.logFunc(level, msg, fields)
	}
}

// isTransientReason returns true if the error reason is transient and worth
// retrying on the same candidate (rate_limit, overloaded).
// Timeouts are NOT transient: a 120s timeout means the endpoint is down,
// and retrying burns another 120s. Immediate fallback is better.
func isTransientReason(reason FailoverReason) bool {
	return reason == FailoverRateLimit || reason == FailoverOverloaded
}

// backoffWait waits for the given duration, respecting context cancellation.
func backoffWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ResolveCandidates parses model config into a deduplicated candidate list.
func ResolveCandidates(cfg ModelConfig, defaultProvider string) []FallbackCandidate {
	seen := make(map[string]bool)
	var candidates []FallbackCandidate

	addCandidate := func(raw string) {
		ref := ParseModelRef(raw, defaultProvider)
		if ref == nil {
			return
		}
		key := ModelKey(ref.Provider, ref.Model)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, FallbackCandidate{
			Provider: ref.Provider,
			Model:    ref.Model,
		})
	}

	// Primary first.
	addCandidate(cfg.Primary)

	// Then fallbacks.
	for _, fb := range cfg.Fallbacks {
		addCandidate(fb)
	}

	return candidates
}

// Execute runs the fallback chain for text/chat requests.
// It tries each candidate in order, respecting cooldowns and error classification.
//
// Behavior:
//   - The primary candidate (index 0) in cooldown is skipped (logged as skipped attempt).
//   - context.Canceled aborts immediately (user abort, no fallback).
//   - Non-retriable errors (format) abort immediately.
//   - Transient retriable errors (rate_limit, overloaded) retry with backoff.
//   - Timeout errors trigger immediate fallback (no retry on same candidate).
//   - Non-transient retriable errors trigger fallback to next candidate.
//   - Success marks provider as good (resets cooldown).
//   - If all fail, returns aggregate error with all attempts.
func (fc *FallbackChain) Execute(
	ctx context.Context,
	candidates []FallbackCandidate,
	run func(ctx context.Context, provider, model string) (*LLMResponse, error),
) (*FallbackResult, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("fallback: no candidates configured")
	}

	fc.log("info", fmt.Sprintf("Fallback: starting chain with %d candidates", len(candidates)),
		map[string]any{"candidates": len(candidates)})

	result := &FallbackResult{
		Attempts: make([]FallbackAttempt, 0, len(candidates)),
	}

	for i, candidate := range candidates {
		// Check context before each attempt.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Check cooldown (per-model: one failing model doesn't block others on the same provider).
		modelKey := ModelKey(candidate.Provider, candidate.Model)
		if i == 0 && !fc.cooldown.IsAvailable(modelKey) {
			remaining := fc.cooldown.CooldownRemaining(modelKey)
			fc.log("info", fmt.Sprintf("Fallback: skipping %s/%s (cooldown, %s remaining)",
				candidate.Provider, candidate.Model, remaining.Round(time.Second)),
				map[string]any{"provider": candidate.Provider, "model": candidate.Model, "remaining": remaining.String()})
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Skipped:  true,
				Reason:   FailoverRateLimit,
				Error: fmt.Errorf(
					"provider %s in cooldown (%s remaining)",
					candidate.Provider,
					remaining.Round(time.Second),
				),
			})
			continue
		}

		fc.log("info", fmt.Sprintf("Fallback: trying candidate %d/%d %s/%s",
			i+1, len(candidates), candidate.Provider, candidate.Model),
			map[string]any{"provider": candidate.Provider, "model": candidate.Model, "index": i})

		// Attempt with retry loop for transient errors.
		start := time.Now()
		retries := 0
		backoff := fc.retry.InitialBackoff
		var lastReason FailoverReason

		for attempt := 0; ; attempt++ {
			if attempt > 0 {
				// Backoff before retry.
				fc.log("info", fmt.Sprintf("Fallback: retrying %s/%s (attempt %d/%d, backoff %s, reason=%s)",
					candidate.Provider, candidate.Model, attempt+1, fc.retry.MaxRetries+1, backoff, lastReason),
					map[string]any{
						"provider": candidate.Provider,
						"model":    candidate.Model,
						"attempt":  attempt + 1,
						"backoff":  backoff.String(),
						"reason":   string(lastReason),
					})
				if err := backoffWait(ctx, backoff); err != nil {
					result.Attempts = append(result.Attempts, FallbackAttempt{
						Provider: candidate.Provider,
						Model:    candidate.Model,
						Error:    err,
						Reason:   lastReason,
						Duration: time.Since(start),
						Retries:  retries,
					})
					return nil, ctx.Err()
				}
				backoff = time.Duration(float64(backoff) * fc.retry.BackoffFactor)
				if backoff > fc.retry.MaxBackoff {
					backoff = fc.retry.MaxBackoff
				}
				retries++
			}

			resp, err := run(ctx, candidate.Provider, candidate.Model)

			if err == nil {
				// Success.
				fc.cooldown.MarkSuccess(modelKey)
				result.Response = resp
				result.Provider = candidate.Provider
				result.Model = candidate.Model
				return result, nil
			}

			// Context cancellation or deadline: abort immediately, no fallback.
			if ctx.Err() != nil {
				result.Attempts = append(result.Attempts, FallbackAttempt{
					Provider: candidate.Provider,
					Model:    candidate.Model,
					Error:    err,
					Duration: time.Since(start),
					Retries:  retries,
				})
				return nil, ctx.Err()
			}

			// Classify the error.
			failErr := ClassifyError(err, candidate.Provider, candidate.Model)

			if failErr == nil {
				// Unclassifiable error: do not fallback, return immediately.
				result.Attempts = append(result.Attempts, FallbackAttempt{
					Provider: candidate.Provider,
					Model:    candidate.Model,
					Error:    err,
					Duration: time.Since(start),
					Retries:  retries,
				})
				return nil, fmt.Errorf("fallback: unclassified error from %s/%s: %w",
					candidate.Provider, candidate.Model, err)
			}

			// Non-retriable error: abort immediately.
			if !failErr.IsRetriable() {
				result.Attempts = append(result.Attempts, FallbackAttempt{
					Provider: candidate.Provider,
					Model:    candidate.Model,
					Error:    failErr,
					Reason:   failErr.Reason,
					Duration: time.Since(start),
					Retries:  retries,
				})
				return nil, failErr
			}

			lastReason = failErr.Reason

			// Transient + retries available: retry same candidate.
			if isTransientReason(failErr.Reason) && attempt < fc.retry.MaxRetries {
				continue
			}

			// Retries exhausted or non-transient retriable error: move to next candidate.
			elapsed := time.Since(start)
			fc.cooldown.MarkFailure(modelKey, failErr.Reason)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    failErr,
				Reason:   failErr.Reason,
				Duration: elapsed,
				Retries:  retries,
			})
			fc.log("warn", fmt.Sprintf("Fallback: candidate %s/%s failed (reason=%s, %s)",
				candidate.Provider, candidate.Model, failErr.Reason, elapsed.Round(time.Millisecond)),
				map[string]any{
					"provider": candidate.Provider,
					"model":    candidate.Model,
					"reason":   string(failErr.Reason),
					"duration": elapsed.String(),
					"retries":  retries,
					"error":    failErr.Error(),
				})
			break // move to next candidate
		}

		// If this was the last candidate, return aggregate error.
		if i == len(candidates)-1 {
			fc.log("error", fmt.Sprintf("Fallback: all %d candidates exhausted", len(candidates)),
				map[string]any{"candidates": len(candidates), "attempts": len(result.Attempts)})
			return nil, &FallbackExhaustedError{Attempts: result.Attempts}
		}
	}

	// All candidates were skipped or failed.
	fc.log("error", fmt.Sprintf("Fallback: all %d candidates exhausted", len(candidates)),
		map[string]any{"candidates": len(candidates), "attempts": len(result.Attempts)})
	return nil, &FallbackExhaustedError{Attempts: result.Attempts}
}

// ExecuteImage runs the fallback chain for image/vision requests.
// Simpler than Execute: no cooldown checks (image endpoints have different rate limits).
// Image dimension/size errors abort immediately (non-retriable).
func (fc *FallbackChain) ExecuteImage(
	ctx context.Context,
	candidates []FallbackCandidate,
	run func(ctx context.Context, provider, model string) (*LLMResponse, error),
) (*FallbackResult, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("image fallback: no candidates configured")
	}

	fc.log("info", fmt.Sprintf("Fallback: starting image chain with %d candidates", len(candidates)),
		map[string]any{"candidates": len(candidates)})

	result := &FallbackResult{
		Attempts: make([]FallbackAttempt, 0, len(candidates)),
	}

	for i, candidate := range candidates {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		fc.log("info", fmt.Sprintf("Fallback: trying image candidate %d/%d %s/%s",
			i+1, len(candidates), candidate.Provider, candidate.Model),
			map[string]any{"provider": candidate.Provider, "model": candidate.Model, "index": i})

		start := time.Now()
		resp, err := run(ctx, candidate.Provider, candidate.Model)
		elapsed := time.Since(start)

		if err == nil {
			result.Response = resp
			result.Provider = candidate.Provider
			result.Model = candidate.Model
			return result, nil
		}

		if ctx.Err() != nil {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Duration: elapsed,
			})
			return nil, ctx.Err()
		}

		// Image dimension/size errors are non-retriable.
		errMsg := strings.ToLower(err.Error())
		if IsImageDimensionError(errMsg) || IsImageSizeError(errMsg) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Error:    err,
				Reason:   FailoverFormat,
				Duration: elapsed,
			})
			return nil, &FailoverError{
				Reason:   FailoverFormat,
				Provider: candidate.Provider,
				Model:    candidate.Model,
				Wrapped:  err,
			}
		}

		// Any other error: record and try next.
		result.Attempts = append(result.Attempts, FallbackAttempt{
			Provider: candidate.Provider,
			Model:    candidate.Model,
			Error:    err,
			Duration: elapsed,
		})
		fc.log("warn", fmt.Sprintf("Fallback: image candidate %s/%s failed (%s)",
			candidate.Provider, candidate.Model, elapsed.Round(time.Millisecond)),
			map[string]any{
				"provider": candidate.Provider,
				"model":    candidate.Model,
				"duration": elapsed.String(),
				"error":    err.Error(),
			})

		if i == len(candidates)-1 {
			fc.log("error", fmt.Sprintf("Fallback: all %d image candidates exhausted", len(candidates)),
				map[string]any{"candidates": len(candidates), "attempts": len(result.Attempts)})
			return nil, &FallbackExhaustedError{Attempts: result.Attempts}
		}
	}

	fc.log("error", fmt.Sprintf("Fallback: all %d image candidates exhausted", len(candidates)),
		map[string]any{"candidates": len(candidates), "attempts": len(result.Attempts)})
	return nil, &FallbackExhaustedError{Attempts: result.Attempts}
}

// FallbackExhaustedError indicates all fallback candidates were tried and failed.
type FallbackExhaustedError struct {
	Attempts []FallbackAttempt
}

func (e *FallbackExhaustedError) Error() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("fallback: all %d candidates failed:", len(e.Attempts)))
	for i, a := range e.Attempts {
		if a.Skipped {
			sb.WriteString(fmt.Sprintf("\n  [%d] %s/%s: skipped (cooldown)", i+1, a.Provider, a.Model))
		} else if a.Retries > 0 {
			sb.WriteString(fmt.Sprintf("\n  [%d] %s/%s: %v (reason=%s, %s, retries=%d)",
				i+1, a.Provider, a.Model, a.Error, a.Reason, a.Duration.Round(time.Millisecond), a.Retries))
		} else {
			sb.WriteString(fmt.Sprintf("\n  [%d] %s/%s: %v (reason=%s, %s)",
				i+1, a.Provider, a.Model, a.Error, a.Reason, a.Duration.Round(time.Millisecond)))
		}
	}
	return sb.String()
}
