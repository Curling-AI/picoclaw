package commands

import (
	"context"
	"fmt"
)

func startCommand() Definition {
	return Definition{
		Name:        "start",
		Description: "Start the bot",
		Usage:       "/start",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			// Dynamic bot name (fork 6b0f535e): use the configured display name,
			// falling back to "PicoClaw".
			botName := "PicoClaw"
			if rt != nil && rt.Config != nil {
				if n := rt.Config.Agents.Defaults.Name; n != "" {
					botName = n
				}
			}
			return req.Reply(fmt.Sprintf("Hello! I am %s 🦞", botName))
		},
	}
}
