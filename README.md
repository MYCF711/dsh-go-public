# DSH-GO

A Go implementation of the [DeepSeek Harness](https://github.com/deepseek-ai) **host**.
It provides the Transport, sessions, tools, approvals, jobs and model routing that
the official `dsh-tui` can drive with `--backend dsh-go`.

**Status: `0.2.2-go.1`** (upstream baseline `0.2.2-rc.1`). This is the first
public GO release; the TUI/desktop surface is still being completed.

## Build

```bash
go build ./...
go test ./...
go vet ./...
```

Windows release binary:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/dshd.exe ./cmd/dshd
```

## Run

```bash
dshd serve --home <home> --addr 127.0.0.1:0 --policy ask
# ready {"url":"http://127.0.0.1:<port>","token":"<token>"}
```

Transport: `POST /api/<method>`, one-shot `GET /connect?token=`, per-session `WS /ws`.
Tool approvals default to `ask`; use `--policy allow|deny` explicitly in
unattended runs.

### dsh-TUI

```bash
dsh-tui --backend dsh-go
```

The TUI plugin bundle is [dsh-tui2go](https://github.com/MYCF711/dsh-tui2go).

## Architecture

```text
cmd/dshd              host entry (serve / run / acp / schedule)
internal/kernel       plugin runtime; no business types
internal/spi          module contracts
internal/transport    HTTP /api + WS /ws, per-session journal
internal/session      v4 append-only session log
modules/              tools, fs, bash, scheduler, goal, ...
codecs/               v4 codecs
```

Three rules: the kernel does not know business types; modules receive only a
`Scope`; only Transport crosses the wire.

## Compatibility

The official 0.2.2 desktop API has 193 methods. A method is only “implemented” when it has a real implementation or a
structured unsupported error, never fake data or a silent success.

## License

MIT
