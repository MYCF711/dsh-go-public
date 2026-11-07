package transport

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// captureStderr 捕获 fn 执行期间的 stderr。测试串行运行，不并行。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = old
		_ = r.Close()
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAPILogDefaultsToErrorsOnly(t *testing.T) {
	s := newTestServer(t, Options{
		List: func() ([]SessionSummary, error) {
			return []SessionSummary{{ID: "a", Title: "t", CWD: "/w", UpdatedAt: "2026-01-01"}}, nil
		},
		Delete: func(string) error { return ErrNoSuchSession },
	})

	success := captureStderr(t, func() {
		if rec := postJSON(t, s, "/api/session.list", ""); rec.Code != http.StatusOK {
			t.Fatalf("success status=%d", rec.Code)
		}
	})
	if strings.Contains(success, "transport: /api/session.list") {
		t.Fatalf("成功调用默认不应刷 stderr，得到 %q", success)
	}

	failure := captureStderr(t, func() {
		if rec := postJSON(t, s, "/api/session.delete", `{"sessionId":"missing"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("failure status=%d", rec.Code)
		}
	})
	if !strings.Contains(failure, "transport: /api/session.delete status=404") {
		t.Fatalf("失败调用必须打 status，得到 %q", failure)
	}
	if strings.Contains(failure, "transport: /api/session.delete\n") {
		t.Fatalf("失败调用不应再打无状态旧格式，得到 %q", failure)
	}
}

func TestAPITraceLogsSuccessfulCalls(t *testing.T) {
	t.Setenv("DSHD_TRACE_API", "1")
	s := newTestServer(t, Options{
		List: func() ([]SessionSummary, error) {
			return []SessionSummary{{ID: "a", Title: "t", CWD: "/w", UpdatedAt: "2026-01-01"}}, nil
		},
	})

	out := captureStderr(t, func() {
		if rec := postJSON(t, s, "/api/session.list", ""); rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
	})
	if !strings.Contains(out, "transport: /api/session.list\n") {
		t.Fatalf("trace 必须打成功调用，得到 %q", out)
	}
	if strings.Contains(out, "status=200") {
		t.Fatalf("成功调用不应打 status，得到 %q", out)
	}
}

func TestAPILogRecords400FromHandler(t *testing.T) {
	s := newTestServer(t, Options{Delete: func(string) error { return errors.New("boom") }})
	out := captureStderr(t, func() {
		if rec := postJSON(t, s, "/api/session.delete", `{"sessionId":"x"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d", rec.Code)
		}
	})
	if !strings.Contains(out, "transport: /api/session.delete status=500") {
		t.Fatalf("500 必须记录 status，得到 %q", out)
	}
}
