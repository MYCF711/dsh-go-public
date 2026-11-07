package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLoadStatsPreservesMissingAndEmptyEffort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		result  string
		present bool
		empty   bool
	}{
		{name: "缺失", result: `{}`, present: false},
		{name: "空值", result: `{"effort":""}`, present: true, empty: true},
		{name: "真值", result: `{"effort":"xhigh"}`, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := effortHTTPServer(t, func(sid string) string { return tc.result })
			defer srv.Close()
			cl := &client{base: srv.URL, hc: srv.Client()}
			st, err := cl.loadStats("session-A")
			if err != nil {
				t.Fatal(err)
			}
			field := reflect.ValueOf(st).FieldByName("Effort")
			if !field.IsValid() {
				t.Fatal("stats 没有 Effort 可选字段，缺失与空值无法区分")
			}
			if got := !field.IsNil(); got != tc.present {
				t.Fatalf("Effort presence=%v，期望 %v", got, tc.present)
			}
			if tc.present {
				got := field.Elem().String()
				if tc.empty && got != "" {
					t.Fatalf("空值被改成 %q", got)
				}
				if !tc.empty && got != "xhigh" {
					t.Fatalf("真值被改成 %q", got)
				}
			}
		})
	}
}

func TestSwitchSessionEffortDoesNotLeak(t *testing.T) {
	results := map[string]string{
		"session-A": `{"effort":"high","model":"model-A"}`,
		"session-B": `{"model":"model-B"}`,
	}
	srv := effortHTTPServer(t, func(sid string) string { return results[sid] })
	defer srv.Close()
	cl := &client{base: srv.URL, hc: srv.Client()}
	cl.dialSubscription = func(string, int) (subscriptionConn, error) {
		return newFakeSubscriptionConn(), nil
	}
	a := newTestApp(120, 24)
	a.cl = cl
	defer func() {
		if a.closeSubscription != nil {
			a.closeSubscription()
		}
	}()

	if err := a.switchTo(sessionRow{ID: "session-A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if row := a.statusRow(currentTheme(), 0, 0).plain(); !strings.Contains(row, "model-A · high effort") {
		t.Fatalf("A 的真实 effort 没上状态行：%q", row)
	}
	if err := a.switchTo(sessionRow{ID: "session-B", Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if row := a.statusRow(currentTheme(), 0, 0).plain(); !strings.Contains(row, "model-B · ? effort") || strings.Contains(row, "high effort") {
		t.Fatalf("切到无记录的 B 后串入 A 的 effort：%q", row)
	}
	results["session-A"] = `{"effort":"","model":"model-A"}`
	if err := a.switchTo(sessionRow{ID: "session-A", Title: "A"}); err != nil {
		t.Fatal(err)
	}
	if row := a.statusRow(currentTheme(), 0, 0).plain(); !strings.Contains(row, "model-A · ? effort") {
		t.Fatalf("显式空 effort 应显示未知：%q", row)
	}
}

func TestKnownEffortStatusRowsStayWithinWidth(t *testing.T) {
	for _, size := range [][2]int{{120, 36}, {80, 24}} {
		a := newTestApp(size[0], size[1])
		a.model = "model-A"
		a.effort = "xhigh"
		rows := a.view()
		if len(rows) != size[1] {
			t.Fatalf("%dx%d 行数=%d", size[0], size[1], len(rows))
		}
		for i, row := range rows {
			if got := dispWidth(row); got > size[0] {
				t.Fatalf("%dx%d 第%d行溢出 %d：%q", size[0], size[1], i, got, row)
			}
		}
		if row := rows[len(rows)-2]; !strings.Contains(row, "model-A · xhigh effort") {
			t.Fatalf("%dx%d 状态行没有真实 effort：%q", size[0], size[1], row)
		}
	}
}

func effortHTTPServer(t *testing.T, result func(string) string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session.load" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		var req struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"result":%s}`, result(req.ID))
	}))
}
