# DSH-GO 0.2.2-go.1

首个公开 GO 版本。

## 宿主

- HTTP `POST /api/<method>` + 每会话 `WS /ws` Transport。
- v4 append-only 会话、history 重放、journal epoch/reset。
- 工具、审批、取消、后台任务、调度、目标、压缩、模型路由。
- 启动：`dshd serve --home <dir> --addr 127.0.0.1:0 --policy ask`。

## TUI

- 配合 [`dsh-tui2go`](https://github.com/MYCF711/dsh-tui2go) 使用
  `dsh-tui --backend dsh-go`。

## 已知边界

- 桌面 API 覆盖尚未完整；未实现项返回结构化 unsupported，不返回假数据。
- TUI 0.19 的 profile 启动当前被上游 `workspace:*` 坏发布阻塞；宿主与
  bundle 本身可构建、可通过各自检查。
- OS 级沙盒未实现；审批与路径边界不是完整沙盒。

## 构建

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/dshd.exe ./cmd/dshd
```
