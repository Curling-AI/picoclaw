package providers

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

// Errors from a gateway that owns a wallet. These are account conditions, not
// provider failures, and the generic classifier gets both of them wrong on its
// own: it reads 429 as a retriable rate limit and does not classify 404 at all.
// (seucaranguejo fork)

// balanceBody is the exact envelope measured against the live gateway: 122
// characters, with the code as the LAST field.
const balanceBody = `{"error":{"message":"You exceeded your current credit balance","type":"insufficient_quota","code":"insufficient_balance"}}`

func httpErr(status int, body, code string) *common.HTTPError {
	preview := body
	if len(preview) > 128 {
		preview = preview[:128] + "..."
	}
	return &common.HTTPError{StatusCode: status, BodyPreview: preview, ErrorCode: code}
}

func TestInsufficientCreditIsNotARateLimit(t *testing.T) {
	err := httpErr(http.StatusTooManyRequests, balanceBody, "insufficient_balance")
	if !IsInsufficientCreditError(err) {
		t.Fatal("balance 429 not recognized")
	}
	// The whole point: billing is non-retriable, rate_limit is not. Retrying
	// does not make money appear — it spends two more requests and delays the
	// actionable message by ~6s.
	failErr := ClassifyError(err, "hulk", "ethos-pro")
	if failErr == nil || failErr.Reason != FailoverBilling {
		t.Fatalf("classified as %v, want billing", failErr)
	}
}

func TestOrdinaryRateLimitStillRetries(t *testing.T) {
	// A throttling 429 must keep its retriable classification, or a transient
	// blip becomes a hard failure for the user.
	err := httpErr(
		http.StatusTooManyRequests,
		`{"error":{"message":"Rate limit reached for requests","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
		"rate_limit_exceeded",
	)
	if IsInsufficientCreditError(err) {
		t.Error("ordinary rate limit misread as exhausted credit")
	}
	failErr := ClassifyError(err, "p", "m")
	if failErr == nil || failErr.Reason != FailoverRateLimit {
		t.Fatalf("classified as %v, want rate_limit", failErr)
	}
}

func TestInsufficientCreditRequiresThe429(t *testing.T) {
	// Matching the message alone would let a provider that merely mentions
	// "quota" while throttling be read as a billing failure.
	err := httpErr(http.StatusServiceUnavailable,
		`{"error":{"message":"insufficient_quota on upstream","code":"unavailable"}}`, "unavailable")
	if IsInsufficientCreditError(err) {
		t.Error("non-429 body mentioning quota treated as exhausted credit")
	}
}

func TestInsufficientCreditSurvivesTruncationAndWrapping(t *testing.T) {
	// The typed code is the reliable path; these are the fallbacks.
	//
	// Truncated preview: with a longer message the trailing code is cut off,
	// which is exactly why classification cannot depend on it.
	long := `{"error":{"message":"You exceeded your current credit balance for this organization and billing period, please top up","type":"insufficient_quota","code":"insufficient_balance"}}`
	truncated := httpErr(http.StatusTooManyRequests, long, "")
	if len(truncated.BodyPreview) > 131 {
		t.Fatalf("preview not truncated: %d chars", len(truncated.BodyPreview))
	}
	if !IsInsufficientCreditError(truncated) {
		t.Error("truncated preview lost the classification")
	}

	// Already wrapped by the retry loop, with the typed error gone.
	wrapped := fmt.Errorf("LLM call failed after retries: %w",
		errors.New("API request failed:\n  Status: 429\n  Body:   "+balanceBody))
	if !IsInsufficientCreditError(wrapped) {
		t.Error("wrapped plain error lost the classification")
	}
}

func TestUnknownGatewayUserIsRecognized(t *testing.T) {
	// The gateway serves its catalog from a snapshot rebuilt about once a
	// minute, so a freshly linked account can 404 on its first message.
	err := httpErr(
		http.StatusNotFound,
		`{"error":{"message":"The user is not active for this product","type":"invalid_request_error","code":"user_not_found"}}`,
		"user_not_found",
	)
	if !IsUnknownGatewayUserError(err) {
		t.Fatal("user_not_found not recognized")
	}
	// A missing MODEL is a configuration bug and must not be confused with it:
	// retrying that would only delay a failure that never heals.
	modelErr := httpErr(
		http.StatusNotFound,
		`{"error":{"message":"The requested model does not exist or is inactive","type":"invalid_request_error","code":"model_not_found"}}`,
		"model_not_found",
	)
	if IsUnknownGatewayUserError(modelErr) {
		t.Error("model_not_found misread as an unknown user")
	}
}

func TestNilAndUnrelatedErrorsAreSafe(t *testing.T) {
	if IsInsufficientCreditError(nil) || IsUnknownGatewayUserError(nil) {
		t.Error("nil error classified")
	}
	plain := errors.New("connection reset by peer")
	if IsInsufficientCreditError(plain) || IsUnknownGatewayUserError(plain) {
		t.Error("transport error classified as an account condition")
	}
}
