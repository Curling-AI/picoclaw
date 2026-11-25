# Notes for AI agents

Before changing `pkg/memory`, `pkg/session`, or anything that writes a workspace's `sessions/`, read `pkg/memory/doc.go`.

- Two processes share `sessions/` during a rollout (old and new pod).
- Never write a session file without its session lock, the flock in `sessions/.locks/`. `memory.JSONLStore`, the fallback `session.SessionManager` and the agent's file tools take it; a new writer outside `pkg/memory` takes it through `memory/sessionlock`. Recovery runs only in `memory.JSONLStore`.
- Never trust a cache another process may change without rereading it under the lock.
- The `TestTwoStore*` tests in `pkg/memory` pin this; keep them passing.
