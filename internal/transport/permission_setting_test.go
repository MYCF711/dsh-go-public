package transport

import (
	"errors"
	"net/http"
	"testing"
)

// handleSessionSetPermission 是**权限分支**：它决定这次改策略放不放行。
// 所以它必须有一条测试钉着（scripts/perm-gates.json 里登记的就是这一条）。
//
// 三条判据：
//   - 只认 deny/ask/allow/auto（空串 = 清除覆盖）
//   - 不认识的值直接 400，且说清合法值——不能静默当成某个默认
//   - 宿主没接线时 501，不是"改了但没生效"
func TestHandleSessionSetPermission(t *testing.T) {
	var gotSID, gotPerm string
	s := newTestServer(t, Options{
		SetSessionPermission: func(sid, perm string) error {
			gotSID, gotPerm = sid, perm
			return nil
		},
	})

	// 合法值：四种 + 空串清除
	for _, perm := range []string{"deny", "ask", "allow", "auto", ""} {
		gotSID, gotPerm = "", ""
		body := `{"id":"s-1","permission":"` + perm + `"}`
		if rec := postJSON(t, s, "/api/session.setPermission", body); rec.Code != http.StatusOK {
			t.Fatalf("%q 应当是合法值，实际 HTTP %d：%s", perm, rec.Code, rec.Body.String())
		}
		if gotSID != "s-1" || gotPerm != perm {
			t.Fatalf("透传错了：id=%q perm=%q", gotSID, gotPerm)
		}
	}

	// 不认识的值必须 400，而且不许落到下游
	gotSID = ""
	rec := postJSON(t, s, "/api/session.setPermission", `{"id":"s-1","permission":"yolo"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("不认识的值应当 400，实际 %d", rec.Code)
	}
	if gotSID != "" {
		t.Fatal("非法值不该被透传到宿主侧")
	}

	// 下游报错要变成 400 并带上原因
	sErr := newTestServer(t, Options{
		SetSessionPermission: func(string, string) error { return errors.New("这个会话不支持改") },
	})
	if rec := postJSON(t, sErr, "/api/session.setPermission", `{"id":"s","permission":"ask"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("下游报错应当 400，实际 %d", rec.Code)
	}

	// 没接线 → 501
	sNil := newTestServer(t, Options{})
	if rec := postJSON(t, sNil, "/api/session.setPermission", `{"id":"s","permission":"ask"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("没接线应当 501，实际 %d", rec.Code)
	}
}
