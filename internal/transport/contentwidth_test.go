package transport

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// emptyLoad 是"这个会话什么都没有"的最小实现，只为走到 contentWidth 那一行。
func emptyLoad(string, int) (LoadResult, error) { return LoadResult{}, nil }

// H12：contentWidth 只有三种情况，一种都不许含糊。
//
//	不传（0）  → 字段根本不出现，界面自己算
//	传 748     → 原样透传
//	传负数     → 当成"不知道"，不当成"-1 列宽"画出来
//
// 第三条是关键：负数喂给布局引擎通常不是报错，而是画出一个宽度为负的容器——
// 界面看起来"怪但没崩"，这种 bug 没人会去查宿主。
func TestContentWidthContract(t *testing.T) {
	cases := []struct {
		name     string
		width    int
		wantKeys bool
		wantVal  int
	}{
		{"不传（0=不知道）", 0, false, 0},
		{"传 748", 748, true, 748},
		{"传负数按未知处理", -20, false, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t, Options{ContentWidth: c.width, Load: emptyLoad})
			rec := postJSON(t, s, "/api/session.load", `{"id":"s1"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("要 200，拿到 %d：%s", rec.Code, rec.Body.String())
			}
			var raw map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("不是 JSON：%v", err)
			}
			res, _ := raw["result"].(map[string]any)
			if res == nil {
				t.Fatalf("缺 result 信封：%s", rec.Body.String())
			}
			got, present := res["contentWidth"]
			if present != c.wantKeys {
				t.Fatalf("contentWidth 出现与否不对：present=%v，期望 %v（正文 %s）",
					present, c.wantKeys, rec.Body.String())
			}
			if present {
				if int(got.(float64)) != c.wantVal {
					t.Fatalf("contentWidth=%v，期望 %d", got, c.wantVal)
				}
			}
		})
	}
}

// H12：界面拿到的必须是**数字**，不是字符串"748"——
// 字符串会让界面那一侧的类型断言静默失败，然后退回默认宽度，看起来像"宿主没给"。
func TestContentWidthIsNumberNotString(t *testing.T) {
	s := newTestServer(t, Options{ContentWidth: 748, Load: emptyLoad})
	rec := postJSON(t, s, "/api/session.load", `{"id":"s1"}`)
	if !strings.Contains(rec.Body.String(), `"contentWidth":748`) {
		t.Fatalf("应当是裸数字：%s", rec.Body.String())
	}
}
