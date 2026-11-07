# AGENTS.md · DSH-GO

Agent-facing orientation. Read this before changing code.

## What this is

A Go host for DeepSeek Harness. The TUI/desktop is a separate repository; this
repository owns the host, Transport, sessions, tools, approvals, jobs and model
routing.

## Commands

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
go build ./...
go vet ./...
go test ./...
```

Targeted tests first; full `go test ./...` before handoff. A green unit test is
not a real host run: for Transport changes, start `dshd serve --addr 127.0.0.1:0`
and drive real HTTP/WS.

## Architecture

```text
cmd/dshd              entrypoint
internal/kernel       plugin runtime; no business types
internal/spi          module contracts
internal/transport    /api + /ws + per-session journal
internal/session      v4 append-only log (the only durable truth)
modules/              business capabilities
codecs/               v4 codecs
```

Hard rules:

- `internal/kernel` must not import agent/tool/session/llm/business modules.
- Modules receive `*Scope`, never `*App` or globals; no `init()` registration.
- Tools return structured bounded output; errors must keep `Hint`.
- v4 logs are append-only; projections are read-only.
- The host does not embed a JS runtime and does not fork the desktop app.
- Only Transport crosses the host/UI boundary.

## Verification

- `go test ./...`, `go vet ./...`, `go build ./...` must exit 0.
- `scripts/bin-check.py` before running a stale binary; never trust an old exe.
- Do not `taskkill` by image name, reset another worktree, or edit another
  agent's files.
- For tool/approval/terminal changes, test a real `dshd` process and clean up
  only PIDs you started.

## Public repository

Keep this repository clean: no local absolute paths, credentials, private logs,
internal task ledgers or generated binaries. Release assets are attached to
GitHub Releases; `dist/` is not committed.
