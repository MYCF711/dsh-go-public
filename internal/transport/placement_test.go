package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const placementTestCookie = "placement-test-cookie"

type placementHTTPResponse struct {
	status int
	raw    []byte
	top    map[string]json.RawMessage
}

func newPlacementHTTPServer(t *testing.T, opt Options) *Server {
	t.Helper()
	s, err := NewServer(opt)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.mu.Lock()
	s.cookies[placementTestCookie] = struct{}{}
	s.mu.Unlock()
	return s
}

func postPlacement(t *testing.T, s *Server, method, body string) placementHTTPResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: placementTestCookie})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	resp := placementHTTPResponse{
		status: rec.Code,
		raw:    rec.Body.Bytes(),
		top:    make(map[string]json.RawMessage),
	}
	if err := json.Unmarshal(resp.raw, &resp.top); err != nil {
		t.Fatalf("%s 返回非 JSON（status=%d）：%v；body=%q", method, resp.status, err, resp.raw)
	}
	return resp
}

func placementResult(t *testing.T, resp placementHTTPResponse) map[string]any {
	t.Helper()
	if resp.status != http.StatusOK {
		t.Fatalf("成功路径 status=%d，want 200；body=%s", resp.status, resp.raw)
	}
	if len(resp.top) != 1 || resp.top["result"] == nil {
		t.Fatalf("成功响应必须只有 result 顶层键；body=%s", resp.raw)
	}
	var result map[string]any
	if err := json.Unmarshal(resp.top["result"], &result); err != nil {
		t.Fatalf("result 不是对象：%v；body=%s", err, resp.raw)
	}
	return result
}

func requirePlacementError(t *testing.T, resp placementHTTPResponse, wantStatus int, wantCode string) {
	t.Helper()
	if resp.status != wantStatus {
		t.Errorf("status=%d，want %d；body=%s", resp.status, wantStatus, resp.raw)
	}
	if len(resp.top) != 1 || resp.top["error"] == nil {
		t.Errorf("错误响应必须只有 error 顶层键；body=%s", resp.raw)
		return
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(resp.top["error"], &object); err != nil {
		t.Errorf("error 不是对象：%v；body=%s", err, resp.raw)
		return
	}
	wantKeys := []string{"code", "message", "hint", "retryable"}
	if len(object) != len(wantKeys) {
		t.Errorf("error 必须恰好四字段 %v；body=%s", wantKeys, resp.raw)
	}
	for _, key := range wantKeys {
		if _, ok := object[key]; !ok {
			t.Errorf("error 缺字段 %q；body=%s", key, resp.raw)
		}
	}
	var code, message, hint string
	if err := json.Unmarshal(object["code"], &code); err != nil || code != wantCode {
		t.Errorf("error.code=%q（err=%v），want %q；body=%s", code, err, wantCode, resp.raw)
	}
	if err := json.Unmarshal(object["message"], &message); err != nil || message == "" {
		t.Errorf("error.message 必须是非空字符串（%q，err=%v）；body=%s", message, err, resp.raw)
	}
	if err := json.Unmarshal(object["hint"], &hint); err != nil || hint == "" {
		t.Errorf("error.hint 必须是非空字符串（%q，err=%v）；body=%s", hint, err, resp.raw)
	}
	var retryable bool
	if err := json.Unmarshal(object["retryable"], &retryable); err != nil || retryable {
		t.Errorf("error.retryable 必须是 false（%v，err=%v）；body=%s", retryable, err, resp.raw)
	}
}

func TestPromptPlacementForwardsTextAndExactResult(t *testing.T) {
	var called bool
	var legacyCalled bool
	var gotSession, gotText, gotPlacement string
	s := newPlacementHTTPServer(t, Options{
		Prompt: func(string, string) (PromptResult, error) {
			legacyCalled = true
			return PromptResult{}, nil
		},
		PromptPlacement: func(sessionID, text, placement string) (PromptResult, error) {
			called = true
			gotSession, gotText, gotPlacement = sessionID, text, placement
			return PromptResult{Accepted: true, TurnID: "turn-followup-1"}, nil
		},
	})

	resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-A","text":"第一行\n第二行","placement":"followup"}`)
	got := placementResult(t, resp)
	want := map[string]any{"accepted": true, "turnId": "turn-followup-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prompt result=%#v，want %#v；body=%s", got, want, resp.raw)
	}
	if !called || gotSession != "session-A" || gotText != "第一行\n第二行" || gotPlacement != "followup" {
		t.Errorf("PromptPlacement 转发 called=%v session=%q text=%q placement=%q", called, gotSession, gotText, gotPlacement)
	}
	if legacyCalled {
		t.Error("带 placement 的文本请求不得改走旧 Prompt")
	}
}

func TestPromptPlacementDefaultKeepsLegacyPrompt(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "缺省", body: `{"sessionId":"session-legacy","text":"legacy"}`},
		{name: "显式空串", body: `{"sessionId":"session-legacy","text":"legacy","placement":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var legacyCalls, placementCalls int
			var gotSession, gotText string
			s := newPlacementHTTPServer(t, Options{
				Prompt: func(sessionID, text string) (PromptResult, error) {
					legacyCalls++
					gotSession, gotText = sessionID, text
					return PromptResult{Accepted: true, TurnID: "turn-legacy"}, nil
				},
				PromptPlacement: func(string, string, string) (PromptResult, error) {
					placementCalls++
					return PromptResult{}, nil
				},
			})
			resp := postPlacement(t, s, "session.prompt", tt.body)
			got := placementResult(t, resp)
			want := map[string]any{"accepted": true, "turnId": "turn-legacy"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("旧 Prompt result 必须保持原形状：got=%#v want=%#v；body=%s", got, want, resp.raw)
			}
			if legacyCalls != 1 || placementCalls != 0 || gotSession != "session-legacy" || gotText != "legacy" {
				t.Errorf("缺省 placement 必须走旧 Prompt：legacy=%d placement=%d session=%q text=%q", legacyCalls, placementCalls, gotSession, gotText)
			}
		})
	}
}

func TestPromptBlocksPlacementForwardsOrderedContent(t *testing.T) {
	wantBlocks := []ContentBlock{
		{Type: "text", Text: "before"},
		{Type: "attachment", AttachmentID: "att-1"},
		{Type: "reference", ReferenceType: "file", Path: "C:/work/a.txt"},
		{Type: "text", Text: "after"},
	}
	var called bool
	var oldCalls int
	var gotSession, gotPlacement string
	var gotBlocks []ContentBlock
	s := newPlacementHTTPServer(t, Options{
		Prompt: func(string, string) (PromptResult, error) {
			oldCalls++
			return PromptResult{}, nil
		},
		PromptBlocks: func(string, []ContentBlock) (PromptResult, error) {
			oldCalls++
			return PromptResult{}, nil
		},
		PromptPlacement: func(string, string, string) (PromptResult, error) {
			oldCalls++
			return PromptResult{}, nil
		},
		PromptBlocksPlacement: func(sessionID string, blocks []ContentBlock, placement string) (PromptResult, error) {
			called = true
			gotSession, gotPlacement = sessionID, placement
			gotBlocks = append([]ContentBlock(nil), blocks...)
			return PromptResult{Accepted: true, TurnID: "turn-blocks"}, nil
		},
	})

	resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-blocks","content":[{"type":"text","text":"before"},{"type":"attachment","attachmentId":"att-1"},{"type":"reference","referenceType":"file","path":"C:/work/a.txt"},{"type":"text","text":"after"}],"placement":"followup"}`)
	got := placementResult(t, resp)
	want := map[string]any{"accepted": true, "turnId": "turn-blocks"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content prompt result=%#v，want %#v；body=%s", got, want, resp.raw)
	}
	if !called || gotSession != "session-blocks" || gotPlacement != "followup" || !reflect.DeepEqual(gotBlocks, wantBlocks) {
		t.Errorf("PromptBlocksPlacement 转发 called=%v session=%q placement=%q blocks=%#v", called, gotSession, gotPlacement, gotBlocks)
	}
	if oldCalls != 0 {
		t.Errorf("带 content+placement 不得落入旧回调，旧回调调用次数=%d", oldCalls)
	}
}

func TestPromptPlacementLegacyHostRejectsNonTurn(t *testing.T) {
	tests := []struct {
		placement string
		want      int
	}{
		{placement: "turn", want: http.StatusOK},
		{placement: "steer", want: http.StatusNotImplemented},
		{placement: "followup", want: http.StatusNotImplemented},
		{placement: "now", want: http.StatusNotImplemented},
	}
	for _, tt := range tests {
		t.Run(tt.placement, func(t *testing.T) {
			var legacyCalls int
			s := newPlacementHTTPServer(t, Options{
				Prompt: func(sessionID, text string) (PromptResult, error) {
					legacyCalls++
					return PromptResult{Accepted: true, TurnID: "turn-old-host"}, nil
				},
			})
			resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-old","text":"do not misroute","placement":"`+tt.placement+`"}`)
			if tt.want == http.StatusOK {
				got := placementResult(t, resp)
				want := map[string]any{"accepted": true, "turnId": "turn-old-host"}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("显式 turn 的旧宿主结果=%#v，want %#v；body=%s", got, want, resp.raw)
				}
				if legacyCalls != 1 {
					t.Errorf("显式 turn 应保持旧 Prompt，调用次数=%d", legacyCalls)
				}
				return
			}
			requirePlacementError(t, resp, http.StatusNotImplemented, "not_implemented")
			if legacyCalls != 0 {
				t.Errorf("旧宿主收到非 turn placement=%q 时必须 501，不得静默当 turn；旧 Prompt 调用次数=%d", tt.placement, legacyCalls)
			}
		})
	}
}

func TestPromptPlacementInvalidReturns400(t *testing.T) {
	t.Run("非法值不得落入旧 Prompt", func(t *testing.T) {
		var legacyCalls int
		s := newPlacementHTTPServer(t, Options{
			Prompt: func(string, string) (PromptResult, error) {
				legacyCalls++
				return PromptResult{}, nil
			},
		})
		resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-old","text":"bad","placement":"sideways"}`)
		requirePlacementError(t, resp, http.StatusBadRequest, "bad_request")
		if legacyCalls != 0 {
			t.Errorf("非法 placement 不得静默当 turn；旧 Prompt 调用次数=%d", legacyCalls)
		}
	})

	t.Run("宿主 sentinel 映射", func(t *testing.T) {
		var gotPlacement string
		s := newPlacementHTTPServer(t, Options{
			PromptPlacement: func(_, _, placement string) (PromptResult, error) {
				gotPlacement = placement
				return PromptResult{}, ErrPlacementInvalid
			},
		})
		resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-a","text":"bad","placement":"steer"}`)
		requirePlacementError(t, resp, http.StatusBadRequest, "bad_request")
		if gotPlacement != "steer" {
			t.Errorf("宿主收到的 placement=%q", gotPlacement)
		}
	})
}

func TestPromptPlacementUnsupportedSteerReturns501(t *testing.T) {
	var gotSession, gotText, gotPlacement string
	s := newPlacementHTTPServer(t, Options{
		PromptPlacement: func(sessionID, text, placement string) (PromptResult, error) {
			gotSession, gotText, gotPlacement = sessionID, text, placement
			return PromptResult{}, ErrPlacementUnsupported
		},
	})
	resp := postPlacement(t, s, "session.prompt", `{"sessionId":"session-steer","text":"change direction","placement":"steer"}`)
	requirePlacementError(t, resp, http.StatusNotImplemented, "not_implemented")
	if gotSession != "session-steer" || gotText != "change direction" || gotPlacement != "steer" {
		t.Errorf("unsupported steer 必须先原样转发：session=%q text=%q placement=%q", gotSession, gotText, gotPlacement)
	}
}

func TestCancelPlacementForwardsMetadataAndExactResult(t *testing.T) {
	var called bool
	var legacyCalls int
	var gotSession, gotCause, gotTurnID string
	s := newPlacementHTTPServer(t, Options{
		Cancel: func(string) error {
			legacyCalls++
			return nil
		},
		CancelPlacement: func(sessionID, cause, turnID string) (CancelResult, error) {
			called = true
			gotSession, gotCause, gotTurnID = sessionID, cause, turnID
			return CancelResult{
				Cancelled:       true,
				TurnID:          "turn-cancel-9",
				CancelledQueued: []string{"queued-1", "queued-2"},
				StillQueued:     []string{"queued-3"},
				Outcome:         "cancelled",
			}, nil
		},
	})

	resp := postPlacement(t, s, "session.cancel", `{"sessionId":"session-cancel","cause":"user /stop","turnId":"turn-cancel-9"}`)
	got := placementResult(t, resp)
	want := map[string]any{
		"cancelled":       true,
		"turnId":          "turn-cancel-9",
		"cancelledQueued": []any{"queued-1", "queued-2"},
		"stillQueued":     []any{"queued-3"},
		"outcome":         "cancelled",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cancel result=%#v，want %#v；body=%s", got, want, resp.raw)
	}
	if !called || gotSession != "session-cancel" || gotCause != "user /stop" || gotTurnID != "turn-cancel-9" {
		t.Errorf("CancelPlacement 转发 called=%v session=%q cause=%q turnId=%q", called, gotSession, gotCause, gotTurnID)
	}
	if legacyCalls != 0 {
		t.Errorf("新 cancel 已接线时不得改走旧 Cancel，调用次数=%d", legacyCalls)
	}
}

func TestCancelPlacementLegacyCancelCompatible(t *testing.T) {
	var gotSession string
	var legacyCalls int
	s := newPlacementHTTPServer(t, Options{
		Cancel: func(sessionID string) error {
			legacyCalls++
			gotSession = sessionID
			return nil
		},
	})
	resp := postPlacement(t, s, "session.cancel", `{"sessionId":"session-legacy-cancel"}`)
	got := placementResult(t, resp)
	want := map[string]any{"cancelled": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("旧 Cancel 的 HTTP result 必须保持原形状：got=%#v want=%#v；body=%s", got, want, resp.raw)
	}
	if legacyCalls != 1 || gotSession != "session-legacy-cancel" {
		t.Errorf("旧 Cancel 兼容失败：calls=%d session=%q", legacyCalls, gotSession)
	}
}
