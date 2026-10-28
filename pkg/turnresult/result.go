// Package turnresult exposes synchronous executor outcomes without interpreting
// response text or relying on the lossy runtime event bus.
package turnresult

import (
	"context"
	"errors"
)

type Status string

const (
	Completed     Status = "completed"
	Failed        Status = "failed"
	Canceled      Status = "canceled"
	Indeterminate Status = "indeterminate"
)

type Result struct {
	Status  Status
	Content string
}

type Request struct {
	Progress   func(stage string)
	Content    string
	SessionKey string
	Channel    string
	ChatID     string
	Media      []string
	// Silent: o turno não publica nada no chat sozinho (sem texto de fallback,
	// feedback de ferramenta ou stream) e uma resposta vazia é um fim normal,
	// não uma falha. Só a ferramenta message entrega. (seucaranguejo fork)
	Silent bool
}

func Failure(err error) Result {
	if errors.Is(err, context.Canceled) {
		return Result{Status: Canceled, Content: ""}
	}
	return Result{Status: Failed, Content: ""}
}
