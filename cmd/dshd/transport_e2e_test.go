package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// TestU0EndToEnd 是 U0 的验收：**模拟界面**把整条链路走一遍，把真实 JSON 打出来。
//
// 不是"测试全绿"，是"界面拿得到它要的东西"：
//  1. /connect 用 launch token 换 cookie（token 一次性）
//  2. POST /api/session.list → 左栏要的字段（id/title/cwd/updatedAt）
//  3. WS 升级 → sub → 真的收到一帧
func TestU0EndToEnd(t *testing.T) {
	home := os.Getenv("DSH_TEST_HOME")
	if home == "" {
		t.Skip("没设 DSH_TEST_HOME")
	}
	home = strings.ReplaceAll(home, "\\", "/")

	srv, err := transport.NewServer(transport.Options{
		List: func() ([]transport.SessionSummary, error) {
			ls, err := session.ListSessions(home, "")
			if err != nil {
				return nil, err
			}
			out := make([]transport.SessionSummary, 0, len(ls))
			for _, l := range ls {
				title := strings.TrimSpace(l.Summary)
				if title == "" {
					title = "(无标题)"
				}
				out = append(out, transport.SessionSummary{
					ID: l.SessionID, Title: title, CWD: filepath.ToSlash(l.Cwd),
					UpdatedAt: l.UpdatedAt.Format(time.RFC3339),
				})
			}
			return out, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	t.Logf("服务起在 %s（端口由 OS 分配）", base)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	// ① token 换 cookie
	resp, err := client.Get(base + "/connect?token=" + srv.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("换 cookie 失败: %d", resp.StatusCode)
	}
	t.Logf("① /connect → 303，拿到 cookie %s…", resp.Cookies()[0].Name)

	// ② 列会话
	r2, err := client.Post(base+"/api/session.list", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("session.list 要 200，拿到 %d：%s", r2.StatusCode, body)
	}
	var payload struct {
		Sessions []transport.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("正文不是 JSON: %v（%s）", err, body)
	}
	if len(payload.Sessions) == 0 {
		t.Fatal("左栏一条会话都没有")
	}
	first := payload.Sessions[0]
	t.Logf("② /api/session.list → %d 条，第一条: %+v", len(payload.Sessions), first)
	if first.ID == "" || first.Title == "" || first.UpdatedAt == "" {
		t.Fatalf("左栏要的字段不全: %+v", first)
	}

	// ③ WS 升级 → sub → 收到一帧
	u, _ := url.Parse(base)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	cookie := resp.Cookies()[0]
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nCookie: %s=%s\r\n\r\n",
		u.Host, key, cookie.Name, cookie.Value)
	br := bufio.NewReader(conn)
	hs, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hs.StatusCode != 101 {
		t.Fatalf("升级要 101，拿到 %d", hs.StatusCode)
	}
	want := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if got := hs.Header.Get("Sec-WebSocket-Accept"); got != base64.StdEncoding.EncodeToString(want[:]) {
		t.Fatalf("Accept 不对: %s", got)
	}
	t.Logf("③ /ws → 101，Accept 校验通过")

	// 发 sub（带 mask）
	payloadText := []byte(`{"t":"sub","topic":"session:` + first.ID + `","sinceSeq":0}`)
	mask := [4]byte{1, 2, 3, 4}
	frame := []byte{0x81, byte(0x80 | len(payloadText))}
	frame = append(frame, mask[:]...)
	for i, b := range payloadText {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// 宿主推一帧（真实场景由 turn 触发）
	srv.Publish("session:"+first.ID, transport.ServerFrame{Seq: 1, Type: "user/message"})

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	h1, _ := br.ReadByte()
	h2, _ := br.ReadByte()
	n := int(h2 & 0x7f)
	buf := make([]byte, n)
	io.ReadFull(br, buf)
	if h1 != 0x81 || h2&0x80 != 0 {
		t.Fatalf("服务端帧头不对: %#x %#x", h1, h2)
	}
	t.Logf("④ WS 收到: %s", buf)
	if !strings.Contains(string(buf), `"seq":1`) {
		t.Fatalf("推的帧没收到: %s", buf)
	}
}

// TestU0LoadAndHistory 钉住另外两个 method：首屏与断流补洞。
//
// 界面点开一个会话要立刻看到最后几条；断流重连要按 sinceSeq 补，
// 而且**补洞不能重复投递**（重复比丢帧更难查）。
func TestU0LoadAndHistory(t *testing.T) {
	home := os.Getenv("DSH_TEST_HOME")
	sid := os.Getenv("DSH_TEST_SESSION_ID")
	if home == "" || sid == "" {
		t.Skip("需要 DSH_TEST_HOME 与 DSH_TEST_SESSION_ID")
	}
	home = strings.ReplaceAll(home, "\\", "/")

	src := newSessionSource(home, "")
	srv, err := transport.NewServer(transport.Options{
		Load: func(id string, limit int) (transport.LoadResult, error) {
			return transport.LoadSession(src, id, limit)
		},
		History: func(id string, sinceSeq, limit int) (transport.Page, error) {
			return transport.HistorySession(src, id, sinceSeq, limit)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	if r, err := c.Get(base + "/connect?token=" + srv.Token()); err != nil {
		t.Fatal(err)
	} else {
		r.Body.Close()
	}

	post := func(method string, body any) (int, []byte) {
		b, _ := json.Marshal(body)
		resp, err := c.Post(base+"/api/"+method, "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}

	// ① 首屏
	code, body := post("session.load", map[string]any{"sessionId": sid, "limit": 5})
	if code != 200 {
		t.Fatalf("session.load 要 200，拿到 %d：%s", code, body)
	}
	var lr transport.LoadResult
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if len(lr.Messages) == 0 || len(lr.Messages) > 5 {
		t.Fatalf("首屏该给 1..5 条，拿到 %d", len(lr.Messages))
	}
	t.Logf("① session.load → %d 条（total=%d, truncated=%v）最后一条: %s",
		len(lr.Messages), lr.Total, lr.Truncated, lr.Messages[len(lr.Messages)-1].Role)

	lastSeq := lr.Messages[len(lr.Messages)-1].Seq
	// ② 从最后一条之后补洞 → 必须空
	code, body = post("session.history", map[string]any{"sessionId": sid, "sinceSeq": lastSeq, "limit": 50})
	if code != 200 {
		t.Fatalf("session.history 要 200，拿到 %d：%s", code, body)
	}
	var pg transport.Page
	var raw map[string]json.RawMessage
	json.Unmarshal(body, &raw)
	if err := json.Unmarshal(body, &pg); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if len(pg.Messages) != 0 {
		t.Fatalf("从最后一条之后补洞应当为空，拿到 %d 条", len(pg.Messages))
	}
	t.Logf("② session.history(sinceSeq=%d) → 0 条（没有重复投递）", lastSeq)

	// ③ 从 0 补洞 → 必须拿到东西，且 seq 严格递增
	code, body = post("session.history", map[string]any{"sessionId": sid, "sinceSeq": 0, "limit": 50})
	if code != 200 {
		t.Fatalf("session.history 要 200，拿到 %d", code)
	}
	json.Unmarshal(body, &pg)
	if len(pg.Messages) == 0 {
		t.Fatal("从 0 补洞应当有内容")
	}
	for i := 1; i < len(pg.Messages); i++ {
		if pg.Messages[i].Seq <= pg.Messages[i-1].Seq {
			t.Fatalf("seq 不是严格递增：%d → %d", pg.Messages[i-1].Seq, pg.Messages[i].Seq)
		}
	}
	t.Logf("③ session.history(sinceSeq=0) → %d 条，seq 严格递增", len(pg.Messages))
}
