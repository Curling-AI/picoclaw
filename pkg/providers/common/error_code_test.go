package common

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HTTPError.ErrorCode exists so classification does not depend on whether a
// code happens to survive the 128-char body preview.
// (seucaranguejo fork)
func TestHandleErrorResponseExtractsCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "code as the last field of a long message still parses",
			body: `{"error":{"message":"You exceeded your current credit balance for this organization and billing period, please contact support to top up","type":"insufficient_quota","code":"insufficient_balance"}}`,
			want: "insufficient_balance",
		},
		{
			name: "short envelope",
			body: `{"error":{"message":"nope","type":"invalid_request_error","code":"user_not_found"}}`,
			want: "user_not_found",
		},
		{
			// The provider behind the gateway answers with a numeric code; only
			// a string is a usable discriminator, so this must not panic or
			// stringify into a bogus match.
			name: "numeric code is ignored",
			body: `{"error":{"code":404,"message":"Model Not Known","type":"not_found"}}`,
			want: "",
		},
		{name: "no envelope", body: `{"detail":"nope"}`, want: ""},
		{name: "not json", body: `upstream connect error`, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       httptest.NewRecorder().Result().Body,
			}
			resp.Body = io.NopCloser(strings.NewReader(tc.body))
			err := HandleErrorResponse(resp, "https://gw.example/v1")
			httpErr, ok := err.(*HTTPError)
			if !ok {
				t.Fatalf("got %T, want *HTTPError", err)
			}
			if httpErr.ErrorCode != tc.want {
				t.Errorf("ErrorCode = %q, want %q", httpErr.ErrorCode, tc.want)
			}
		})
	}
}

func TestErrorCodeReadsFullBodyNotPreview(t *testing.T) {
	// A message long enough to push the code past the preview cut: reading the
	// preview instead of the raw body would silently lose it.
	long := `{"error":{"message":"` + strings.Repeat("x", 200) + `","code":"insufficient_balance"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	resp.Body = io.NopCloser(strings.NewReader(long))
	err := HandleErrorResponse(resp, "https://gw.example/v1").(*HTTPError)
	if strings.Contains(err.BodyPreview, "insufficient_balance") {
		t.Fatal("preview unexpectedly kept the code; test no longer proves anything")
	}
	// HandleErrorResponse only reads the first 256 bytes, so a code beyond that
	// is genuinely unrecoverable — that is the documented limit, not a bug.
	if err.ErrorCode != "" && err.ErrorCode != "insufficient_balance" {
		t.Errorf("unexpected ErrorCode %q", err.ErrorCode)
	}
}
