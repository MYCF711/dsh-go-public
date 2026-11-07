package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/spi"
	"dshgo/internal/transport"
)

type w145Source struct {
	home      string
	cwd       string
	id        string
	path      string
	firstSeq  int64
	secondSeq int64
	thirdSeq  int64
	secondID  string
}

func newW145Source(t *testing.T, home, id string, messages int) w145Source {
	t.Helper()
	cwd := filepath.Join(home, "workspace-"+id)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := session.CreateSession(home, cwd, id, "standard")
	if err != nil {
		t.Fatal(err)
	}
	out := w145Source{home: home, cwd: cwd, id: id, path: lg.Path}
	if err := lg.Append("system/message", session.SystemMessageData(1, 1, "system")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < messages; i++ {
		data := session.UserMessage([]spi.Block{{Type: "text", Text: fmt.Sprintf("user-%03d", i)}})
		if err := lg.Append("user/message", data); err != nil {
			t.Fatal(err)
		}
		seq := lg.Seq() - 1
		if i == 0 {
			out.firstSeq = seq
		}
		if i == 1 {
			out.secondSeq = seq
			out.secondID = data["id"].(string)
		}
		if i == 2 {
			out.thirdSeq = seq
		}
		if err := lg.Append("assistant/message", session.AssistantMessageEvent(i+1, 1,
			[]map[string]any{session.TextBlock(fmt.Sprintf("assistant-%03d", i))}, "stop", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestServeForkTitleAnchorAndPlainCompatibility(t *testing.T) {
	home := t.TempDir()
	src := newW145Source(t, home, "source-fork", 3)
	title := "  Keep this title  "
	anchor := strconv.FormatInt(src.secondSeq, 10)
	got, err := serveForkWithOptions(home, src.id, &anchor, &title)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID == "" || got.SessionID == src.id {
		t.Fatalf("fork sessionId=%q", got.SessionID)
	}
	if got.Anchor == nil || *got.Anchor != anchor || got.Title == nil || *got.Title != title {
		t.Fatalf("fork echo=%+v, want exact anchor/title", got)
	}
	childPath, err := newSessionSource(home, "").resolve(got.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := v4.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	nodes := session.Surface(child)
	if len(nodes) != 4 || nodes[3].Message.Content != "user-001" {
		t.Fatalf("inclusive anchor surface=%+v", nodes)
	}
	list, err := listSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range list {
		if row.ID == got.SessionID {
			found = true
			if row.Title != title {
				t.Fatalf("session.list title=%q, want exact %q", row.Title, title)
			}
		}
	}
	if !found {
		t.Fatal("child missing from session.list after reopen")
	}

	plainID, err := serveFork(home, src.id, "")
	if err != nil {
		t.Fatal(err)
	}
	plainPath, err := newSessionSource(home, "").resolve(plainID)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := v4.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(session.Surface(plain)); got != 7 {
		t.Fatalf("plain whole-session fork surface=%d, want 7", got)
	}
	for _, ev := range plain.Events {
		if ev.Type == "session/title" {
			t.Fatal("plain old fork unexpectedly gained a title event")
		}
	}
}

func TestServeForkRejectsInvalidAnchorWithoutCreatingChild(t *testing.T) {
	home := t.TempDir()
	src := newW145Source(t, home, "source-bad-anchor", 2)
	bad := strconv.FormatInt(src.firstSeq+1, 10) // assistant, not user
	if _, err := serveForkWithOptions(home, src.id, &bad, nil); !errors.Is(err, session.ErrInvalidAnchor) {
		t.Fatalf("assistant anchor error=%v, want ErrInvalidAnchor", err)
	}
	list, err := listSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("invalid anchor created children: %+v", list)
	}
}

func TestCaptureRewindCheckpointForLogUsesDurableUserAnchor(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "capture-workspace")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	lg, err := session.CreateSession(home, cwd, "capture-source", "standard")
	if err != nil {
		t.Fatal(err)
	}
	writeW145File(t, cwd, "before.txt", "checkpoint\n")
	if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: "capture"}})); err != nil {
		t.Fatal(err)
	}
	anchor := strconv.FormatInt(lg.Seq()-1, 10)
	if err := captureRewindCheckpointForLog(home, lg); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	writeW145File(t, cwd, "before.txt", "changed later\n")
	preview, err := serveRewindPreview(home, "capture-source", anchor)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.FilesChanged) != 1 || preview.FilesChanged[0] != "before.txt" {
		t.Fatalf("runner-captured checkpoint preview=%+v", preview)
	}
}

func writeW145File(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readW145File(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServeRewindPreviewApplyAndPartialOutcome(t *testing.T) {
	home := t.TempDir()
	src := newW145Source(t, home, "source-rewind", 3)
	writeW145File(t, src.cwd, "src/a.txt", "old\n")
	writeW145File(t, src.cwd, "delete.txt", "restore me\n")
	if err := session.CaptureRewindCheckpoint(home, src.id, src.secondSeq, src.cwd); err != nil {
		t.Fatal(err)
	}
	writeW145File(t, src.cwd, "src/a.txt", "new\n")
	writeW145File(t, src.cwd, "created.txt", "remove me\n")
	if err := os.Remove(filepath.Join(src.cwd, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	anchor := strconv.FormatInt(src.secondSeq, 10)
	preview, err := serveRewindPreview(home, src.id, anchor)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"created.txt", "delete.txt", "src/a.txt"}
	if strings.Join(preview.FilesChanged, ",") != strings.Join(wantFiles, ",") {
		t.Fatalf("preview files=%v, want %v", preview.FilesChanged, wantFiles)
	}
	if readW145File(t, src.cwd, "src/a.txt") != "new\n" {
		t.Fatal("preview modified files")
	}

	conversation, err := serveRewindApply(home, src.id, anchor, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if conversation.Kind != "rewound" || conversation.SessionID == "" || conversation.SessionID == src.id || conversation.Files != nil {
		t.Fatalf("conversation outcome=%+v", conversation)
	}
	childPath, err := newSessionSource(home, "").resolve(conversation.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := v4.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(session.Surface(child)); got != 3 {
		t.Fatalf("conversation child surface=%d, want cut-before-anchor 3", got)
	}

	files, err := serveRewindApply(home, src.id, anchor, "files")
	if err != nil {
		t.Fatal(err)
	}
	if files.Kind != "rewound" || files.SessionID != src.id || files.Files == nil {
		t.Fatalf("files outcome=%+v", files)
	}
	if readW145File(t, src.cwd, "src/a.txt") != "old\n" || readW145File(t, src.cwd, "delete.txt") != "restore me\n" {
		t.Fatal("files were not restored to checkpoint bytes")
	}
	if _, err := os.Stat(filepath.Join(src.cwd, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("post-anchor file still exists, err=%v", err)
	}

	// Recreate a real difference, then force only the conversation half to fail.
	writeW145File(t, src.cwd, "src/a.txt", "new again\n")
	oldFork := rewindForkBefore
	rewindForkBefore = func(*v4.Log, string, string, string, string) (*session.Log, error) {
		return nil, errors.New("fork write failed")
	}
	t.Cleanup(func() { rewindForkBefore = oldFork })
	partial, err := serveRewindApply(home, src.id, anchor, "both")
	if err != nil {
		t.Fatal(err)
	}
	if partial.Kind != "rewound" || partial.SessionID != "" || partial.Files == nil || partial.ConversationError != "fork write failed" {
		t.Fatalf("partial outcome=%+v", partial)
	}
	if readW145File(t, src.cwd, "src/a.txt") != "old\n" {
		t.Fatal("partial outcome did not preserve successful file restoration")
	}
}

func TestServeRewindRefusesInvalidAnchorAndValidatesMode(t *testing.T) {
	home := t.TempDir()
	src := newW145Source(t, home, "source-refused", 2)
	for _, anchor := range []string{"", "999999", strconv.FormatInt(src.firstSeq+1, 10)} {
		if _, err := serveRewindPreview(home, src.id, anchor); err == nil {
			t.Errorf("preview anchor %q unexpectedly succeeded", anchor)
		}
		outcome, err := serveRewindApply(home, src.id, anchor, "both")
		if err != nil {
			t.Fatal(err)
		}
		if outcome.Kind != "refused" || outcome.Reason == "" || outcome.SessionID != "" || outcome.Files != nil {
			t.Fatalf("anchor %q refusal=%+v", anchor, outcome)
		}
	}
	if _, err := serveRewindApply(home, src.id, strconv.FormatInt(src.secondSeq, 10), "invalid"); err == nil {
		t.Fatal("invalid mode must fail before any mutation")
	}
	first, err := serveRewindApply(home, src.id, strconv.FormatInt(src.firstSeq, 10), "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != "refused" {
		t.Fatalf("first-user conversation rewind=%+v, want refused", first)
	}
}

func TestServeTranscriptPageBoundariesRestartABAndLargeLog(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "transcript-a", 0)
	b := newW145Source(t, home, "transcript-b", 0)
	lg, err := session.OpenSessionAt(a.path)
	if err != nil {
		t.Fatal(err)
	}
	const count = 180
	for i := 0; i < count; i++ {
		raw := make([]byte, 24000)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		text := fmt.Sprintf("m-%03d-", i) + base64.RawStdEncoding.EncodeToString(raw)
		if err := lg.Append("user/message", session.UserMessage([]spi.Block{{Type: "text", Text: text}})); err != nil {
			t.Fatal(err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= tailThresholdBytes {
		t.Fatalf("fixture size=%d, want large-log path > %d", st.Size(), tailThresholdBytes)
	}
	baseline, err := transport.LoadSession(newSessionSource(home, ""), a.id, 3)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Total != -1 || !baseline.Truncated || len(baseline.Messages) != 3 {
		t.Fatalf("large baseline=%+v, want truncated tail", baseline)
	}

	var flattened []transport.Message
	cursor := ""
	pages := 0
	lastCursor := ""
	for {
		page, err := serveTranscriptPage(home, a.id, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) == 0 {
			t.Fatal("unexpected empty page before durable exhaustion")
		}
		for i := 1; i < len(page.Messages); i++ {
			if page.Messages[i-1].Seq >= page.Messages[i].Seq {
				t.Fatalf("page is not oldest-first: %+v", page.Messages)
			}
		}
		flattened = append(append([]transport.Message{}, page.Messages...), flattened...)
		lastCursor = page.NextCursor
		if lastCursor == "" || lastCursor == cursor {
			t.Fatalf("non-progressing cursor at page %d: old=%q new=%q", pages, cursor, lastCursor)
		}
		pages++
		if !page.HasOlder {
			break
		}
		cursor = page.NextCursor
		if pages > count {
			t.Fatal("cursor did not terminate")
		}
	}
	if len(flattened) != count {
		t.Fatalf("flattened durable pages=%d, want %d", len(flattened), count)
	}
	for i, msg := range flattened {
		if !strings.HasPrefix(msg.Text, fmt.Sprintf("m-%03d-", i)) {
			t.Fatalf("flattened[%d] prefix=%q", i, msg.Text[:min(20, len(msg.Text))])
		}
	}
	// A new source/server process has no cursor state; the durable cursor still works.
	again, err := serveTranscriptPage(home, a.id, lastCursor, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Messages) != 0 || again.HasOlder || again.NextCursor != lastCursor {
		t.Fatalf("exhausted page is not stable: %+v", again)
	}
	twice, err := serveTranscriptPage(home, a.id, again.NextCursor, 7)
	if err != nil || len(twice.Messages) != 0 || twice.NextCursor != lastCursor {
		t.Fatalf("repeated empty page=%+v err=%v", twice, err)
	}
	if _, err := serveTranscriptPage(home, b.id, lastCursor, 7); err == nil {
		t.Fatal("A cursor must not page B")
	}
	for _, bad := range []string{"not-base64", "e30"} {
		if _, err := serveTranscriptPage(home, a.id, bad, 7); err == nil {
			t.Errorf("invalid cursor %q unexpectedly accepted", bad)
		}
	}
	if _, err := serveTranscriptPage(home, a.id, "", 201); err == nil {
		t.Fatal("unbounded page limit unexpectedly accepted")
	}
}

func TestTranscriptMatchesCompleteBaselineAndInitialReplayCount(t *testing.T) {
	home := t.TempDir()
	src := newW145Source(t, home, "small-transcript", 5)
	source := newSessionSource(home, "")
	baseline, err := transport.LoadSession(source, src.id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Truncated || baseline.Total != len(baseline.Messages) {
		t.Fatalf("small baseline is not complete: %+v", baseline)
	}
	page, err := serveTranscriptPage(home, src.id, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != len(baseline.Messages) {
		t.Fatalf("transcript=%d baseline=%d", len(page.Messages), len(baseline.Messages))
	}
	initialMessages := len(baseline.Messages)
	replayed := append([]transport.Message{}, page.Messages[len(page.Messages)-initialMessages:]...)
	if len(replayed) != baseline.Total || initialMessages != len(replayed) {
		t.Fatalf("initialMessages=%d replayed=%d total=%d", initialMessages, len(replayed), baseline.Total)
	}
}

type w145HTTPServer struct {
	base   string
	client *http.Client
	close  func() error
}

func startW145HTTPServer(t *testing.T, home string) w145HTTPServer {
	t.Helper()
	source := newSessionSource(home, "")
	opts := withSessionOperationOptions(home, transport.Options{
		Source: source,
		List:   func() ([]transport.SessionSummary, error) { return listSessions(home, "") },
		Load: func(id string, limit int) (transport.LoadResult, error) {
			return transport.LoadSession(source, id, limit)
		},
	})
	srv, err := transport.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	base, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(base + "/connect?token=" + srv.Token())
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		srv.Close()
		t.Fatalf("connect status=%d", resp.StatusCode)
	}
	return w145HTTPServer{base: base, client: client, close: srv.Close}
}

func callW145HTTP(t *testing.T, srv w145HTTPServer, method string, params any, wantStatus int) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.client.Post(srv.base+"/api/"+method, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s status=%d, want %d: %s", method, resp.StatusCode, wantStatus, body)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Hint      string `json:"hint"`
			Retryable *bool  `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("%s bad envelope: %v: %s", method, err, body)
	}
	if wantStatus != http.StatusOK {
		if env.Error.Code == "" || env.Error.Message == "" || env.Error.Hint == "" || env.Error.Retryable == nil {
			t.Fatalf("%s malformed error envelope: %s", method, body)
		}
		return nil
	}
	return env.Result
}

func TestSessionOperationsRealHTTPRestartAndAB(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "http-a", 3)
	b := newW145Source(t, home, "http-b", 3)
	writeW145File(t, a.cwd, "a.txt", "before\n")
	if err := session.CaptureRewindCheckpoint(home, a.id, a.secondSeq, a.cwd); err != nil {
		t.Fatal(err)
	}
	writeW145File(t, a.cwd, "a.txt", "after\n")
	first := startW145HTTPServer(t, home)
	anchor := strconv.FormatInt(a.secondSeq, 10)
	title := "  HTTP exact title  "
	var fork transport.ForkResult
	if err := json.Unmarshal(callW145HTTP(t, first, "session.fork", map[string]any{
		"id": a.id, "anchor": anchor, "title": title,
	}, http.StatusOK), &fork); err != nil {
		t.Fatal(err)
	}
	if fork.Anchor == nil || *fork.Anchor != anchor || fork.Title == nil || *fork.Title != title {
		t.Fatalf("HTTP fork=%+v", fork)
	}
	var preview transport.RewindPreview
	if err := json.Unmarshal(callW145HTTP(t, first, "session.rewind.preview", map[string]any{
		"id": a.id, "anchor": anchor,
	}, http.StatusOK), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.FilesChanged) != 1 || preview.FilesChanged[0] != "a.txt" {
		t.Fatalf("HTTP preview=%+v", preview)
	}
	var page transport.TranscriptPageResult
	if err := json.Unmarshal(callW145HTTP(t, first, "session.transcript.page", map[string]any{
		"id": a.id, "limit": 4,
	}, http.StatusOK), &page); err != nil {
		t.Fatal(err)
	}
	if !page.HasOlder || len(page.Messages) != 4 || page.NextCursor == "" {
		t.Fatalf("first HTTP page=%+v", page)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}

	// New listener, cookie and Source: only the same durable home survives.
	second := startW145HTTPServer(t, home)
	defer second.close()
	var listed struct {
		Sessions []transport.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(callW145HTTP(t, second, "session.list", map[string]any{}, http.StatusOK), &listed); err != nil {
		t.Fatal(err)
	}
	titleFound := false
	for _, row := range listed.Sessions {
		if row.ID == fork.SessionID && row.Title == title {
			titleFound = true
		}
	}
	if !titleFound {
		t.Fatalf("restarted session.list lost exact title: %+v", listed.Sessions)
	}
	var older transport.TranscriptPageResult
	if err := json.Unmarshal(callW145HTTP(t, second, "session.transcript.page", map[string]any{
		"id": a.id, "cursor": page.NextCursor, "limit": 4,
	}, http.StatusOK), &older); err != nil {
		t.Fatal(err)
	}
	if older.HasOlder || len(older.Messages) != 2 || older.NextCursor == "" {
		t.Fatalf("restarted older page=%+v", older)
	}
	var empty transport.TranscriptPageResult
	if err := json.Unmarshal(callW145HTTP(t, second, "session.transcript.page", map[string]any{
		"id": a.id, "cursor": older.NextCursor, "limit": 4,
	}, http.StatusOK), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Messages) != 0 || empty.HasOlder || empty.NextCursor != older.NextCursor {
		t.Fatalf("restarted stable empty page=%+v", empty)
	}
	callW145HTTP(t, second, "session.transcript.page", map[string]any{
		"id": b.id, "cursor": page.NextCursor, "limit": 4,
	}, http.StatusBadRequest)
	var rewound transport.RewindResult
	if err := json.Unmarshal(callW145HTTP(t, second, "session.rewind.apply", map[string]any{
		"id": a.id, "anchor": anchor, "mode": "files",
	}, http.StatusOK), &rewound); err != nil {
		t.Fatal(err)
	}
	if rewound.Kind != "rewound" || rewound.SessionID != a.id || rewound.Files == nil {
		t.Fatalf("HTTP files rewind=%+v", rewound)
	}
	if got := readW145File(t, a.cwd, "a.txt"); got != "before\n" {
		t.Fatalf("restarted file restore=%q", got)
	}
}
