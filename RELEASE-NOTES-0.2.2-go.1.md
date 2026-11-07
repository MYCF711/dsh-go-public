# DSH-GO 0.2.2-go.1

First public GO release.

## Host

- HTTP `POST /api/<method>` and per-session `WS /ws` Transport.
- v4 append-only sessions, history replay and journal epoch/reset.
- Tools, approvals, cancellation, jobs, scheduler, goals, compaction and model
  routing.
- `dshd serve --home <dir> --addr 127.0.0.1:0 --policy ask`.

## TUI

- Works with [`dsh-tui2go`](https://github.com/MYCF711/dsh-tui2go) and
  `dsh-tui --backend dsh-go`.

## Known gaps

- Desktop API coverage is not complete; see `docs/host-stable-gap-register.md`.
- TUI 0.19 profile bootstrap currently hits upstream broken `workspace:*`
  publishes; the host and bundle themselves build and pass their checks.
- OS-level sandboxing is policy-only; approvals and path boundaries are not a
  full sandbox.
