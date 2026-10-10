package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestCallOutcomeUnknown(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "no error", err: nil, want: false},
		{name: "answer lost on the way back", err: fmt.Errorf("failed to call tool: %w", io.EOF), want: true},
		{name: "canceled while waiting", err: fmt.Errorf("failed to call tool: %w", context.Canceled), want: true},
		{name: "never sent", err: NotSent(errors.New("manager is closed")), want: false},
		{
			name: "server answered with an error",
			err: fmt.Errorf(
				"failed to call tool: %w",
				&jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "boom"},
			),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CallOutcomeUnknown(tt.err); got != tt.want {
				t.Errorf("CallOutcomeUnknown(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// A call to a server the pod doesn't have, or after the manager closed, never
// reaches a tool.
func TestCallToolFailuresBeforeSendingAreNotUnknown(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.CallTool(context.Background(), "missing", "create", nil)
	if err == nil || CallOutcomeUnknown(err) {
		t.Fatalf("unknown server: err = %v, outcome unknown = %v; want a not-sent error", err, CallOutcomeUnknown(err))
	}
	if err.Error() != "server missing not found" {
		t.Errorf("message = %q, want it unchanged", err.Error())
	}

	if closeErr := mgr.Close(); closeErr != nil {
		t.Fatalf("Close: %v", closeErr)
	}
	_, err = mgr.CallTool(context.Background(), "missing", "create", nil)
	if err == nil || CallOutcomeUnknown(err) {
		t.Fatalf("closed manager: err = %v; want a not-sent error", err)
	}
}
