package transport

// 官方 `subagents.*` 的**可续问子会话**契约测试。
//
// 钉住的语义（`@deepseek-ai/dsh-subagent` 的 SubagentRuntime）：
//   - 未登记子会话 → 响亮失败（no_such_session），**不编** messageId；
//   - 父 id 不匹配 → cross_session 403；
//   - 同一 requestId 重投 → 幂等（同一个 messageId，不投第二次）；
//   - 投递失败 → 错误，且回执里带失败原因；
//   - 没有投递/中断通道 → 501 未实现；
//   - 中断按父授权，accepted 只表示信号被接受。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// childRecorder 记下真实投递与中断。
type childRecorder struct {
	mu         sync.Mutex
	delivered  []string
	placements []string
	interrupts []string
	failNext   error
	created    []string
	persisted  []string
}

func (r *childRecorder) opts() OfficialChildSessionOptions {
	return OfficialChildSessionOptions{
		Create: func(childSessionID, parentSessionID, cwd string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.created = append(r.created, childSessionID+"<-"+parentSessionID)
			return nil
		},
		Deliver: func(_ context.Context, childSessionID string, content []OfficialSubagentPromptContent, placement string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.failNext != nil {
				return r.failNext
			}
			text := ""
			for _, part := range content {
				text += part.Text
			}
			r.delivered = append(r.delivered, childSessionID+":"+text)
			r.placements = append(r.placements, placement)
			return nil
		},
		Interrupt: func(childSessionID string) (bool, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.interrupts = append(r.interrupts, childSessionID)
			return true, nil
		},
		Persist: func(childSessionID, eventType string, data any) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.persisted = append(r.persisted, childSessionID+":"+eventType)
			return nil
		},
	}
}

func newTestChildRegistry(t *testing.T, rec *childRecorder) *OfficialChildRegistry {
	t.Helper()
	registry := NewOfficialChildRegistry(rec.opts())
	if _, err := registry.Spawn("child-1", "parent-1", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return registry
}

func promptReq(requestID, parent, child, delivery string) OfficialSubagentPromptRequest {
	return OfficialSubagentPromptRequest{
		RequestID: requestID, ParentSessionID: parent, ChildSessionID: child,
		Mode: "continuable", Delivery: delivery,
		Content: []OfficialSubagentPromptContent{{Type: "text", Text: "继续"}},
	}
}

func TestOfficialSubagentPromptUnknownChild(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)

	receipt, err := registry.Prompt(context.Background(), promptReq("r1", "parent-1", "child-nope", "queue"))
	if err == nil {
		t.Fatalf("没有子会话竟然成功了：%+v", receipt)
	}
	if !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("应该是 no_such_session，实际 %v", err)
	}
	if receipt.MessageID != "" {
		t.Fatalf("失败时不许带回执：%+v", receipt)
	}
	if len(rec.delivered) != 0 {
		t.Fatalf("失败时不许真的投递：%v", rec.delivered)
	}
}

func TestOfficialSubagentPromptCrossSession(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)

	_, err := registry.Prompt(context.Background(), promptReq("r1", "parent-other", "child-1", "queue"))
	if err == nil {
		t.Fatal("跨会话投递竟然成功了")
	}
	var typed *officialError
	if !errors.As(err, &typed) || typed.status != http.StatusForbidden || typed.code != "cross_session" {
		t.Fatalf("应该是 403 cross_session，实际 %v", err)
	}
	if len(rec.delivered) != 0 {
		t.Fatalf("拒绝时不许真的投递：%v", rec.delivered)
	}
}

func TestOfficialSubagentPromptDelivery(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)

	receipt, err := registry.Prompt(context.Background(), promptReq("r1", "parent-1", "child-1", "queue"))
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.HasPrefix(receipt.MessageID, "msg-") {
		t.Fatalf("messageId 形状不对：%q", receipt.MessageID)
	}
	if len(rec.delivered) != 1 || rec.delivered[0] != "child-1:继续" {
		t.Fatalf("投递内容不对：%v", rec.delivered)
	}
	// queue → PlacementFollowup；steer → PlacementSteer。
	if rec.placements[0] != string(PlacementFollowup) {
		t.Fatalf("queue 应该落 PlacementFollowup，实际 %q", rec.placements[0])
	}
	if _, err := registry.Prompt(context.Background(), promptReq("r2", "parent-1", "child-1", "steer")); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if rec.placements[1] != string(PlacementSteer) {
		t.Fatalf("steer 应该落 PlacementSteer，实际 %q", rec.placements[1])
	}

	// 回执必须落 durable（subagent/message）：每次投递两帧——投递前的身份帧
	// 与投递后的结果帧（delivered / err）。
	if len(rec.persisted) != 4 || rec.persisted[0] != "child-1:subagent/message" {
		t.Fatalf("回执没落盘：%v", rec.persisted)
	}

	child, _ := registry.Lookup("child-1")
	inbox := child.Inbox()
	if len(inbox) != 2 {
		t.Fatalf("inbox 应该有 2 条，实际 %d", len(inbox))
	}
	if !inbox[0].Delivered || inbox[0].RequestID != "r1" {
		t.Fatalf("第一条回执不对：%+v", inbox[0])
	}
}

func TestOfficialSubagentPromptIdempotent(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)

	first, err := registry.Prompt(context.Background(), promptReq("r-dup", "parent-1", "child-1", "queue"))
	if err != nil {
		t.Fatalf("第一次: %v", err)
	}
	second, err := registry.Prompt(context.Background(), promptReq("r-dup", "parent-1", "child-1", "queue"))
	if err != nil {
		t.Fatalf("第二次: %v", err)
	}
	if first.MessageID != second.MessageID {
		t.Fatalf("同一 requestId 应该回同一个 messageId：%q vs %q", first.MessageID, second.MessageID)
	}
	if len(rec.delivered) != 1 {
		t.Fatalf("幂等重投不该投第二次：%v", rec.delivered)
	}
}

func TestOfficialSubagentPromptDeliveryFailureIsLoud(t *testing.T) {
	rec := &childRecorder{failNext: errors.New("prompt 通道坏了")}
	registry := newTestChildRegistry(t, rec)

	receipt, err := registry.Prompt(context.Background(), promptReq("r1", "parent-1", "child-1", "queue"))
	if err == nil {
		t.Fatalf("投递失败竟然回了回执：%+v", receipt)
	}
	if receipt.MessageID != "" {
		t.Fatalf("失败时不许带回执：%+v", receipt)
	}
	child, _ := registry.Lookup("child-1")
	inbox := child.Inbox()
	if len(inbox) != 1 || inbox[0].Err == "" || inbox[0].Delivered {
		t.Fatalf("失败回执不对：%+v", inbox)
	}
}

func TestOfficialSubagentInterrupt(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)

	accepted, err := registry.InterruptByParent("child-1", "parent-1")
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if !accepted.Accepted {
		t.Fatalf("应该 accepted：%+v", accepted)
	}
	if len(rec.interrupts) != 1 || rec.interrupts[0] != "child-1" {
		t.Fatalf("中断没真的打下去：%v", rec.interrupts)
	}

	// 跨会话中断 → 403。
	var typed *officialError
	if _, err := registry.InterruptByParent("child-1", "parent-other"); !errors.As(err, &typed) || typed.code != "cross_session" {
		t.Fatalf("跨会话中断应该是 cross_session，实际 %v", err)
	}
	// 未知子会话 → no_such_session。
	if _, err := registry.InterruptByParent("child-nope", "parent-1"); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("未知子会话应该是 no_such_session，实际 %v", err)
	}
}

func TestOfficialSubagentUnwiredIsLoud(t *testing.T) {
	// 没有投递通道。
	registry := NewOfficialChildRegistry(OfficialChildSessionOptions{})
	if _, err := registry.Spawn("child-1", "parent-1", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, err := registry.Prompt(context.Background(), promptReq("r1", "parent-1", "child-1", "queue")); !errors.Is(err, ErrOfficialNotImplemented) {
		t.Fatalf("没有投递通道应该 501，实际 %v", err)
	}
	// 没有中断通道。
	if _, err := registry.InterruptByParent("child-1", "parent-1"); !errors.Is(err, ErrOfficialNotImplemented) {
		t.Fatalf("没有中断通道应该 501，实际 %v", err)
	}
}

func TestOfficialSubagentSpawnGuards(t *testing.T) {
	registry := NewOfficialChildRegistry(OfficialChildSessionOptions{})
	if _, err := registry.Spawn("", "p", ""); !errors.Is(err, ErrOfficialBadInput) {
		t.Fatalf("空子会话 id 应该 400，实际 %v", err)
	}
	if _, err := registry.Spawn("same", "same", ""); !errors.Is(err, ErrOfficialBadInput) {
		t.Fatalf("子=父应该 400，实际 %v", err)
	}
	if _, err := registry.Spawn("child-1", "parent-1", ""); err != nil {
		t.Fatalf("首次 spawn: %v", err)
	}
	// 同父重复 spawn 幂等。
	if _, err := registry.Spawn("child-1", "parent-1", ""); err != nil {
		t.Fatalf("同父重复 spawn 应该幂等：%v", err)
	}
	// 不同父 → cross_session。
	var typed *officialError
	if _, err := registry.Spawn("child-1", "parent-2", ""); !errors.As(err, &typed) || typed.code != "cross_session" {
		t.Fatalf("换父 spawn 应该 cross_session，实际 %v", err)
	}
}

func TestOfficialSubagentCloseRejectsPrompt(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)
	registry.Close("child-1")

	_, err := registry.Prompt(context.Background(), promptReq("r1", "parent-1", "child-1", "queue"))
	if !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("关闭后的子会话应该是 no_such_session，实际 %v", err)
	}
}

// 走真实 /api 出口：subagents.prompt / interruptByParent 的形状。
func TestOfficialSubagentWire(t *testing.T) {
	rec := &childRecorder{}
	registry := newTestChildRegistry(t, rec)
	srv := officialTestServer(t, OfficialAgentAPI{
		SubagentsPrompt: func(req OfficialSubagentPromptRequest) (OfficialSubagentPromptReceipt, error) {
			return registry.Prompt(context.Background(), req)
		},
		SubagentsInterruptByParent: func(childSessionID, parentSessionID string) (OfficialAccepted, error) {
			return registry.InterruptByParent(childSessionID, parentSessionID)
		},
	})

	code, out := callOfficial(t, srv, "subagents.prompt", map[string]any{
		"request": map[string]any{
			"requestId": "r1", "parentSessionId": "parent-1", "childSessionId": "child-1",
			"mode": "continuable", "delivery": "queue",
			"content": []any{map[string]any{"type": "text", "text": "继续"}},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("subagents.prompt: %d %v", code, out)
	}
	receipt, ok := out["result"].(map[string]any)
	if !ok || !strings.HasPrefix(receipt["messageId"].(string), "msg-") {
		t.Fatalf("回执形状不对：%v", out)
	}

	code, out = callOfficial(t, srv, "subagents.interruptByParent", map[string]any{
		"childSessionId": "child-1", "parentSessionId": "parent-1", "mode": "continuable",
	})
	if code != http.StatusOK {
		t.Fatalf("interruptByParent: %d %v", code, out)
	}
	accepted, ok := out["result"].(map[string]any)
	if !ok || accepted["accepted"] != true {
		t.Fatalf("中断回执不对：%v", out)
	}

	// 未知子会话 → 400 no_such_session 四字段。
	code, out = callOfficial(t, srv, "subagents.interruptByParent", map[string]any{
		"childSessionId": "child-nope", "parentSessionId": "parent-1", "mode": "continuable",
	})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "no_such_session")
}
