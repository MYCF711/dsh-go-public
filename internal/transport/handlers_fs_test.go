package transport

import (
	"net/http"
	"strings"
	"testing"
)

func TestFSReadNotFoundIs404(t *testing.T) {
	s := newTestServer(t, Options{FSRead: func(string) (FSContent, error) { return FSContent{}, ErrNoSuchFile }})
	if rec := postJSON(t, s, "/api/fs.read", `{"path":"nope.txt"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在要 404，拿到 %d", rec.Code)
	}
}

// 路径为空是参数错（400），不是"文件不存在"（404）：混起来界面会以为用户指错了文件。
func TestFSReadEmptyPathIs400(t *testing.T) {
	s := newTestServer(t, Options{FSRead: func(string) (FSContent, error) { t.Fatal("不该被调用"); return FSContent{}, nil }})
	if rec := postJSON(t, s, "/api/fs.read", `{"path":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 path 要 400，拿到 %d", rec.Code)
	}
}

func TestFSReadReturnsContent(t *testing.T) {
	s := newTestServer(t, Options{FSRead: func(p string) (FSContent, error) {
		return FSContent{Path: p, Content: "hello", Size: 5}, nil
	}})
	rec := postJSON(t, s, "/api/fs.read", `{"path":"a.txt"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("要 200 且带正文，拿到 %d %s", rec.Code, rec.Body.String())
	}
}

// 写成功必须明确回 written:true——界面要靠它决定是否清掉"未保存"标记。
func TestFSWriteReturnsWritten(t *testing.T) {
	s := newTestServer(t, Options{FSWrite: func(p, c string) error { return nil }})
	rec := postJSON(t, s, "/api/fs.write", `{"path":"a.txt","content":"x"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"written":true`) {
		t.Fatalf("要 200 且 written:true，拿到 %d %s", rec.Code, rec.Body.String())
	}
}

func TestFSWriteUnwiredIs501(t *testing.T) {
	s := newTestServer(t, Options{})
	if rec := postJSON(t, s, "/api/fs.write", `{"path":"a","content":"b"}`); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线要 501，拿到 %d", rec.Code)
	}
}
