package transport

import (
	"encoding/json"
	"net/http"
	"testing"
)

// H13：审批必须真的能"回"，而且乱回要有明确错误。
func TestApproveUnknownRequestIs404(t *testing.T) {
	s := newTestServer(t, Options{Approve: func(requestID, optionID string) error {
		return ErrNoSuchRequest
	}})
	rec := postJSON(t, s, "/api/session.approve", `{"requestId":"nope","optionId":"allow_once"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 requestId 要 404，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != "no_such_request" {
		t.Fatalf("错误码要是 no_such_request，实际 %q", env.Error.Code)
	}
	if env.Error.Hint == "" {
		t.Fatalf("错误要带 hint")
	}
}

// H13：允许/拒绝必须原样送到宿主（谁执行、执不执行由宿主决定，transport 只转发）。
func TestApproveForwardsDecision(t *testing.T) {
	var gotReq, gotOpt string
	s := newTestServer(t, Options{Approve: func(requestID, optionID string) error {
		gotReq, gotOpt = requestID, optionID
		return nil
	}})
	rec := postJSON(t, s, "/api/session.approve", `{"requestId":"perm-1","optionId":"reject_once"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
	}
	if gotReq != "perm-1" || gotOpt != "reject_once" {
		t.Fatalf("没原样转发：%q %q", gotReq, gotOpt)
	}
}

// H13：不认识的 optionId 要 400，不能被当成"允许"。
func TestApproveRejectsUnknownOption(t *testing.T) {
	s := newTestServer(t, Options{Approve: func(requestID, optionID string) error {
		if optionID != "allow_once" && optionID != "allow_always" && optionID != "reject_once" {
			return ErrBadOption
		}
		return nil
	}})
	rec := postJSON(t, s, "/api/session.approve", `{"requestId":"perm-1","optionId":"yolo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知 optionId 要 400，拿到 %d：%s", rec.Code, rec.Body.String())
	}
}
