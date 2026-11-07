package transport

import (
	"net/http"
	"testing"
)

// 空工作区必须是空数组，不能是 null：界面只该处理一种形状。
func TestWorkspaceListEmptyIsArrayNotNull(t *testing.T) {
	s := newTestServer(t, Options{WorkspaceList: func(path string) ([]WorkspaceEntry, error) { return nil, nil }})
	rec := postJSON(t, s, "/api/workspace.list", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("要 200，拿到 %d", rec.Code)
	}
	got := unwrapResult(t, rec.Body.Bytes())
	entries, ok := got["entries"].([]any)
	if !ok || len(entries) != 0 {
		t.Fatalf("空工作区应当是空数组（不是 null、不是缺键），实际 %v", got)
	}
}

// 没接线 → 501；"还没有这能力"和"它坏了"必须分开。
func TestWorkspaceListUnwiredIs501(t *testing.T) {
	s := newTestServer(t, Options{})
	if rec := postJSON(t, s, "/api/workspace.list", `{}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线要 501，拿到 %d", rec.Code)
	}
}

// 宿主报错要 500 并带原文——"列不动"和"是空的"混起来，用户会以为工作区是空的。
func TestWorkspaceListErrorIs500WithDetail(t *testing.T) {
	s := newTestServer(t, Options{WorkspaceList: func(path string) ([]WorkspaceEntry, error) { return nil, errBoom }})
	rec := postJSON(t, s, "/api/workspace.list", `{}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("要 500，拿到 %d", rec.Code)
	}
	if !contains(rec.Body.String(), "磁盘满了") {
		t.Fatalf("要带宿主原文，实际 %s", rec.Body.String())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
