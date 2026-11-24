# Notes for AI agents

Before changing `pkg/memory`, `pkg/session`, or anything that writes a workspace's `sessions/`, read `pkg/memory/doc.go`.

- Two processes share `sessions/` during a rollout (old and new pod).
- Write and recover session files only through `memory.JSONLStore`, which holds the session's flock in `sessions/.locks/`. Never write a session file directly.
- Never trust a cache another process may change without rereading it under the lock.
- The `TestTwoStore*` tests in `pkg/memory` pin this; keep them passing.
