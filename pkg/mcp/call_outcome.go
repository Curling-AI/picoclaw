package mcp

import (
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// notSentError marks a CallTool failure raised before the request left the
// pod, or after the server reported it had no session for it: the tool did
// not run. (seucaranguejo fork)
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// NotSent wraps a CallTool failure that happened before the request reached
// the tool, keeping its message.
func NotSent(err error) error {
	if err == nil {
		return nil
	}
	return &notSentError{err: err}
}

// CallOutcomeUnknown reports whether a CallTool error leaves open whether the
// tool ran: the request may have reached the server and no answer came back
// (EOF, gateway timeout, canceled context). A failure before sending
// (NotSent) and a JSON-RPC error the server answered with both say it didn't.
// (seucaranguejo fork)
func CallOutcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var notSent *notSentError
	if errors.As(err, &notSent) {
		return false
	}
	var answered *jsonrpc.Error
	return !errors.As(err, &answered)
}
