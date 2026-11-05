package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type diagnosticPublicProjector struct {
	err     error
	invalid bool
}

func (p diagnosticPublicProjector) Commit(message providers.Message) (PublicHistoryProjectionResult, error) {
	if p.invalid {
		message.Role = "tool"
	}
	return PublicHistoryProjectionResult{Message: message, HasVersionedReferences: false}, p.err
}

func (p diagnosticPublicProjector) Legacy(message providers.Message) (PublicHistoryProjectionResult, error) {
	return PublicHistoryProjectionResult{Message: message, HasVersionedReferences: false}, nil
}

func (p diagnosticPublicProjector) Verify(context.Context, providers.Message) error { return nil }

func TestPublicProjectionDiagnosticsPreserveCanonicalContentAndWithholdCoverage(t *testing.T) {
	const secret = "synthetic-private-token"
	const path = "/private/owner/" + secret
	cases := []struct {
		name    string
		err     error
		invalid bool
		class   string
	}{
		{name: "unexpected", err: errors.New("Bearer " + secret), invalid: false, class: "projector_error"},
		{
			name:    "not_found",
			err:     &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist},
			invalid: false,
			class:   "not_found",
		},
		{
			name:    "permission",
			err:     &os.PathError{Op: "read", Path: path, Err: os.ErrPermission},
			invalid: false,
			class:   "permission_denied",
		},
		{name: "canceled", err: fmt.Errorf("%s: %w", secret, context.Canceled), invalid: false, class: "canceled"},
		{
			name:    "deadline",
			err:     fmt.Errorf("%s: %w", secret, context.DeadlineExceeded),
			invalid: false,
			class:   "deadline_exceeded",
		},
		{name: "capacity", err: fmt.Errorf("%s: %w", secret, ErrHistoryCapacity), invalid: false, class: "capacity"},
		{
			name:    "coverage",
			err:     fmt.Errorf("%s: %w", secret, ErrPublicHistoryCoverage),
			invalid: false,
			class:   "coverage_unavailable",
		},
		{
			name:    "incomplete_data",
			err:     fmt.Errorf("%s: %w", secret, io.ErrUnexpectedEOF),
			invalid: false,
			class:   "invalid_data",
		},
		{name: "invalid_projection", err: nil, invalid: true, class: "invalid_projection"},
		{name: "valid", err: nil, invalid: false, class: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			store.SetPublicHistoryProjector(diagnosticPublicProjector{err: test.err, invalid: test.invalid})
			key, content := "agent:private:"+secret, "original content "+secret
			if err := store.AddMessage(t.Context(), key, "assistant", content); err != nil {
				t.Fatal("projection failure discarded a committed message", err)
			}
			history, err := store.GetHistory(t.Context(), key)
			if err != nil || len(history) != 1 || history[0].Content != content ||
				history[0].HistoryPublicMessage != nil {
				t.Fatal("projection failure changed the canonical message")
			}
			public, err := store.ReadPublicHistory(t.Context(), key)
			if test.class == "" {
				if logs.Len() != 0 || err != nil || len(public.Entries) != 1 ||
					public.Entries[0].Message.Content != content {
					t.Fatal("valid projection lost coverage or emitted a warning", err)
				}
				return
			}
			if !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("failed projection claimed complete public coverage", err)
			}
			var diagnostic struct {
				Message   string `json:"msg"`
				Operation string `json:"operation"`
				Class     string `json:"error_class"`
			}
			if err := json.Unmarshal(logs.Bytes(), &diagnostic); err != nil {
				t.Fatal("expected one structured projection warning", err)
			}
			if diagnostic.Message != "public history projection unavailable" ||
				diagnostic.Operation != "commit_public_history" ||
				diagnostic.Class != test.class {
				t.Fatal("projection warning lost its operation or safe cause class", diagnostic)
			}
			for _, private := range []string{secret, key, path, content, "Bearer"} {
				if strings.Contains(logs.String(), private) {
					t.Fatal("projection warning disclosed private data")
				}
			}
		})
	}
}
