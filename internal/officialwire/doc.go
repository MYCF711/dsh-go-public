// Package officialwire 是官方桌面端 0.2.2 的 **L2 wire 协议层**（M1）。
//
// 它只做协议翻译，不认识会话、工具、模型，也不读盘（与 internal/transport
// 同一分层原则：R7 跨线只经 Transport）。真实能力由宿主通过 Binding /
// StreamOpener 注入——cmd/dshd/serve_official_wire.go 把本仓既有方法接上来。
//
// # 它实现的是哪份契约
//
// 逐条对应 L2-WIRE-EVIDENCE.md（下称 EVIDENCE）：
//
//	unary     POST /api/<namespace>/<method>，只收 application/json，
//	          非 POST → 404，非 JSON → 415，body 非 JSON → 400        EVIDENCE §1、§6
//	请求      {"type":"client-request","rpcId",method,payload:{args}} EVIDENCE §2.1
//	响应      {"type":"server-response","rpcId",result:{ok,value|error}}
//	          业务失败也是 HTTP 200                                    EVIDENCE §2.2、§6
//	错误码    gateway/*、remote-access/denied、<namespace>/<reason>    EVIDENCE §2.3
//	mux       /api/remote.mux，七帧，文本帧，二进制 1003              EVIDENCE §3.1
//	背压      上行 256 KiB/流 → gateway/uplink-overflow（不关 socket） EVIDENCE §3.3
//	心跳      2s × 2 次无活动 → terminate                             EVIDENCE §3.3
//	认证      /connect?token=&interactions=none|answer → 302 ./api/remote.mux
//	          + dsh-auth-<b64url(sha256(authority))> 的 v2 HMAC cookie EVIDENCE §4
//	附件      下行 multipart/form-data（metadata + bytes-N）           EVIDENCE §5.1
//
// # 它与既有 L1 的关系（不串语义、不串 seq 域）
//
//   - 路径不重叠：L1 是 `POST /api/<dotted>`（**一段**），L2 是两段 `<ns>/<m>`。
//     Router 只把「恰好两段且每段合法」的 /api 请求交给本包，其余原样交给 L1。
//   - 开关关闭时 Router 直接调用 L1 handler，**不经过本包任何代码**——字节不变
//     是构造性成立的，不是"尽量不改"。
//   - 认证域互不通兑：官方 cookie 只被本包校验；本包调 L1 时用宿主内部服务凭据
//     （Proxy.Legacy），绝不把官方 cookie 当成 L1 cookie。
//   - 序号域：mux 的 item 是**无损 JSON 值**，本包不分配任何序号。映射到 L1
//     journal 时传出去的就是同一批 journal 帧（seq 只有一套）。
//
// # 明确没做的
//
// `$events` 的 waterfall/emit 需要真实的宿主转发事件源，本包只做 ready +
// `$events/result` 的准入判定；没有事件可答时回 gateway/method-unavailable，
// 不伪造成功（EVIDENCE §7.3 明确禁止假数据/假流式）。
package officialwire
