package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// startMetadataInitHTTPServer uses the same production option wiring as cmdServe.
// The first red run intentionally predates withMetadataInitOptions: the three new
// methods must fail on the old host instead of being mocked into existence.
func startMetadataInitHTTPServer(t *testing.T, home string) w145HTTPServer {
	t.Helper()
	source := newSessionSource(home, "")
	journals := transport.NewJournals(nil)
	if _, err := restoreServeJournals(home, journals); err != nil {
		t.Fatal(err)
	}
	opts := withSessionOperationOptions(home, transport.Options{
		Source:   source,
		List:     func() ([]transport.SessionSummary, error) { return listSessions(home, "") },
		Journals: journals,
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
		_ = srv.Close()
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(base + "/connect?token=" + srv.Token())
	if err != nil {
		_ = srv.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		_ = srv.Close()
		t.Fatalf("connect status=%d", resp.StatusCode)
	}
	return w145HTTPServer{base: base, client: client, close: srv.Close}
}

func metadataSessionByID(t *testing.T, srv w145HTTPServer, id string) map[string]any {
	t.Helper()
	raw := callW145HTTP(t, srv, "session.list", map[string]any{}, http.StatusOK)
	var listed struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	var matches []map[string]any
	for _, row := range listed.Sessions {
		if row["id"] == id {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("session.list id=%q rows=%d: %+v", id, len(matches), listed.Sessions)
	}
	return matches[0]
}

func callMetadataHTTPConcurrent(srv w145HTTPServer, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	resp, err := srv.client.Post(srv.base+"/api/"+method, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s status=%d body=%s", method, resp.StatusCode, body)
	}
	return nil
}

func TestSessionListExplicitTitleMetadata(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "title-metadata", 1)
	logw, err := session.OpenSessionAt(a.path)
	if err != nil {
		t.Fatal(err)
	}
	const title = "  exact title metadata  "
	if err := session.AppendSessionTitle(logw, title); err != nil {
		_ = logw.Close()
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	row := metadataSessionByID(t, srv, a.id)
	if row["title"] != title || row["titleExplicit"] != true || row["titleSource"] != "user" {
		t.Fatalf("explicit title row lost durable metadata: %+v", row)
	}
}

func TestSessionRenameHTTPExactDurableReadback(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "rename-a", 1)
	b := newW145Source(t, home, "rename-b", 1)
	first := startMetadataInitHTTPServer(t, home)
	const title = "  exact long title — 不重排不截断  "
	callW145HTTP(t, first, "session.rename", map[string]any{"id": a.id, "title": title}, http.StatusOK)
	row := metadataSessionByID(t, first, a.id)
	if row["title"] != title || row["titleExplicit"] != true || row["titleSource"] != "user" {
		t.Fatalf("rename readback=%+v", row)
	}
	if other := metadataSessionByID(t, first, b.id); other["title"] == title || other["titleExplicit"] == true {
		t.Fatalf("rename leaked A title to B: %+v", other)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	second := startMetadataInitHTTPServer(t, home)
	defer second.close()
	if row := metadataSessionByID(t, second, a.id); row["title"] != title || row["titleExplicit"] != true || row["titleSource"] != "user" {
		t.Fatalf("restart rename readback=%+v", row)
	}
}

func TestSessionColorHTTPDurableABClearRestart(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "color-a", 1)
	b := newW145Source(t, home, "color-b", 1)
	first := startMetadataInitHTTPServer(t, home)
	callW145HTTP(t, first, "session.color.set", map[string]any{"id": a.id, "color": "cyan"}, http.StatusOK)
	var got struct {
		Color string `json:"color"`
	}
	if err := json.Unmarshal(callW145HTTP(t, first, "session.color.get", map[string]any{"id": a.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "cyan" {
		t.Fatalf("A color=%q", got.Color)
	}
	if err := json.Unmarshal(callW145HTTP(t, first, "session.color.get", map[string]any{"id": b.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "" {
		t.Fatalf("B inherited A color=%q", got.Color)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	second := startMetadataInitHTTPServer(t, home)
	if err := json.Unmarshal(callW145HTTP(t, second, "session.color.get", map[string]any{"id": a.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "cyan" {
		t.Fatalf("restart lost A color=%q", got.Color)
	}
	callW145HTTP(t, second, "session.color.set", map[string]any{"id": a.id, "color": ""}, http.StatusOK)
	if err := json.Unmarshal(callW145HTTP(t, second, "session.color.get", map[string]any{"id": a.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "" {
		t.Fatalf("clear left A color=%q", got.Color)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	third := startMetadataInitHTTPServer(t, home)
	defer third.close()
	if err := json.Unmarshal(callW145HTTP(t, third, "session.color.get", map[string]any{"id": a.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "" {
		t.Fatalf("second restart restored cleared A color=%q", got.Color)
	}
	if err := json.Unmarshal(callW145HTTP(t, third, "session.color.get", map[string]any{"id": b.id}, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if got.Color != "" {
		t.Fatalf("A set/clear leaked into B after restart=%q", got.Color)
	}
	var history transport.HistoryResult
	raw := callW145HTTP(t, third, "session.history", map[string]any{"id": a.id, "sinceSeq": 0}, http.StatusOK)
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Frames) != 2 || history.Frames[0].Type != "session/color" || history.Frames[1].Type != "session/color" {
		t.Fatalf("production journal did not survive two restarts: %+v", history.Frames)
	}
}

func TestWorkspaceInitHTTPIdempotentAndNoOverwrite(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	type fileResult struct {
		Path   string `json:"path"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	var report struct {
		Files []fileResult `json:"files"`
	}
	if err := json.Unmarshal(callW145HTTP(t, srv, "workspace.init", map[string]any{"cwd": cwd}, http.StatusOK), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Files) == 0 || report.Files[0].Status != "created" {
		t.Fatalf("first init=%+v", report)
	}
	agents := filepath.Join(cwd, "AGENTS.md")
	firstBytes, err := os.ReadFile(agents)
	if err != nil || string(firstBytes) != workspaceInitTemplate {
		t.Fatalf("created AGENTS bytes=%d err=%v content=%q", len(firstBytes), err, firstBytes)
	}
	const userContent = "user-owned instructions\n"
	if err := os.WriteFile(agents, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	report.Files = nil
	if err := json.Unmarshal(callW145HTTP(t, srv, "workspace.init", map[string]any{"cwd": cwd}, http.StatusOK), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Files) == 0 || report.Files[0].Status != "unchanged" {
		t.Fatalf("second init=%+v", report)
	}
	got, err := os.ReadFile(agents)
	if err != nil || string(got) != userContent {
		t.Fatalf("workspace.init overwrote user file=%q err=%v", got, err)
	}
}

func TestSessionMetadataHTTPDurableEventsAndHistoryOrder(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "metadata-events", 1)
	b := newW145Source(t, home, "metadata-events-b", 1)
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	const title = "  durable title before WS  "
	callW145HTTP(t, srv, "session.rename", map[string]any{"id": a.id, "title": title}, http.StatusOK)
	callW145HTTP(t, srv, "session.color.set", map[string]any{"id": a.id, "color": "cyan"}, http.StatusOK)
	callW145HTTP(t, srv, "session.color.set", map[string]any{"id": a.id, "color": ""}, http.StatusOK)

	var history transport.HistoryResult
	raw := callW145HTTP(t, srv, "session.history", map[string]any{"id": a.id, "sinceSeq": 0}, http.StatusOK)
	if err := json.Unmarshal(raw, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Frames) != 3 {
		t.Fatalf("history frames=%+v", history.Frames)
	}
	wantTypes := []string{"session/title", "session/color", "session/color"}
	for i, frame := range history.Frames {
		if frame.Seq != i+1 || frame.Type != wantTypes[i] || frame.Time <= 0 {
			t.Fatalf("history frame[%d]=%+v", i, frame)
		}
	}
	var titleData struct {
		Title       string  `json:"title"`
		MessageSeqs []int64 `json:"messageSeqs"`
		Source      struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(history.Frames[0].Data, &titleData); err != nil {
		t.Fatal(err)
	}
	if titleData.Title != title || titleData.MessageSeqs == nil || titleData.Source.Kind != "user" {
		t.Fatalf("title frame=%+v", titleData)
	}
	for i, want := range []string{"cyan", ""} {
		var data struct {
			Color string `json:"color"`
		}
		if err := json.Unmarshal(history.Frames[i+1].Data, &data); err != nil || data.Color != want {
			t.Fatalf("color frame[%d]=%+v err=%v", i+1, data, err)
		}
	}

	durable, err := v4.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	var durableTypes []string
	for _, event := range durable.Events {
		if event.Type == "session/title" || event.Type == "session/color" {
			durableTypes = append(durableTypes, event.Type)
		}
	}
	if len(durableTypes) != 3 {
		t.Fatalf("durable metadata events=%v", durableTypes)
	}
	for i := range wantTypes {
		if durableTypes[i] != wantTypes[i] {
			t.Fatalf("durable/journal order=%v/%v", durableTypes, wantTypes)
		}
	}
	other := callW145HTTP(t, srv, "session.history", map[string]any{"id": b.id, "sinceSeq": 0}, http.StatusOK)
	var otherHistory transport.HistoryResult
	if err := json.Unmarshal(other, &otherHistory); err != nil {
		t.Fatal(err)
	}
	if len(otherHistory.Frames) != 0 {
		t.Fatalf("A metadata frames leaked to B: %+v", otherHistory.Frames)
	}
}

func TestSessionColorRealWSConcurrentTailMatchesDurable(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "real-ws-color-a", 1)
	b := newW145Source(t, home, "real-ws-color-b", 1)
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	wsA := dialEffortGoalWS(t, &pluginGlueServer{base: srv.base}, srv.client, a.id, 0, 0)
	defer wsA.Close()
	wsB := dialEffortGoalWS(t, &pluginGlueServer{base: srv.base}, srv.client, b.id, 0, 0)
	defer wsB.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 6; i++ {
		for _, item := range []struct{ id, color string }{{a.id, fmt.Sprintf("a-%d", i)}, {b.id, fmt.Sprintf("b-%d", i)}} {
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := callMetadataHTTPConcurrent(srv, "session.color.set", map[string]any{"id": item.id, "color": item.color}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	readColor := func(ws *effortGoalWS, prefix string) string {
		t.Helper()
		last := ""
		for i := 0; i < 6; i++ {
			frame := readEffortGoalWSFrame(t, ws)
			if frame["type"] != "session/color" || frame["seq"] != float64(i+1) {
				t.Fatalf("WS frame[%d]=%+v", i, frame)
			}
			data := frame["data"].(map[string]any)
			color, ok := data["color"].(string)
			if !ok || !strings.HasPrefix(color, prefix) {
				t.Fatalf("WS color frame=%+v", frame)
			}
			last = color
		}
		return last
	}
	lastA := readColor(wsA, "a-")
	lastB := readColor(wsB, "b-")
	for _, item := range []struct{ id, want string }{{a.id, lastA}, {b.id, lastB}} {
		var got struct {
			Color string `json:"color"`
		}
		if err := json.Unmarshal(callW145HTTP(t, srv, "session.color.get", map[string]any{"id": item.id}, http.StatusOK), &got); err != nil {
			t.Fatal(err)
		}
		if got.Color != item.want {
			t.Fatalf("stale WS tail for %s: WS=%q durable=%q", item.id, item.want, got.Color)
		}
	}
}

func TestSessionColorServiceConcurrentABCommitReadsOwnDurableValue(t *testing.T) {
	home := t.TempDir()
	a := newW145Source(t, home, "concurrent-color-a", 1)
	b := newW145Source(t, home, "concurrent-color-b", 1)
	service := newSessionMetadataService(home)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		for _, item := range []struct {
			id    string
			path  string
			color string
		}{
			{id: a.id, path: a.path, color: fmt.Sprintf("a-%d", i)},
			{id: b.id, path: b.path, color: fmt.Sprintf("b-%d", i)},
		} {
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := service.SetColor(item.id, item.color, func() error {
					got, err := session.ReadSessionColor(item.path)
					if err != nil {
						return err
					}
					if got != item.color {
						return fmt.Errorf("stale commit for %s: durable=%q mutation=%q", item.id, got, item.color)
					}
					return nil
				}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	failures := 0
	for err := range errs {
		failures++
		t.Error(err)
	}
	if failures != 0 {
		t.Fatalf("concurrent mutations succeeded=%d/16", 16-failures)
	}
	for _, item := range []struct {
		path   string
		prefix string
	}{{a.path, "a-"}, {b.path, "b-"}} {
		got, err := session.ReadSessionColor(item.path)
		if err != nil {
			t.Fatal(err)
		}
		valid := false
		for i := 0; i < 8; i++ {
			if got == fmt.Sprintf("%s%d", item.prefix, i) {
				valid = true
			}
		}
		if !valid {
			t.Errorf("final color %q is not one of the requested %s values", got, item.prefix)
		}
	}
}

func TestWorkspaceInitConcurrentCallsHaveCompletedFileForEverySuccess(t *testing.T) {
	service := newSessionMetadataService(t.TempDir())
	cwd := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			report, err := service.WorkspaceInit(cwd)
			if err != nil {
				errs <- err
				return
			}
			if len(report.Files) != 1 || (report.Files[0].Status != "created" && report.Files[0].Status != "unchanged") {
				errs <- fmt.Errorf("unexpected concurrent init report: %+v", report)
				return
			}
			raw, err := os.ReadFile(filepath.Join(cwd, "AGENTS.md"))
			if err != nil || string(raw) != workspaceInitTemplate {
				errs <- fmt.Errorf("success without completed template: bytes=%d err=%v", len(raw), err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestWorkspaceInitHTTPReportsRealFailure(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := startMetadataInitHTTPServer(t, home)
	defer srv.close()
	var report transport.WorkspaceInitResult
	raw := callW145HTTP(t, srv, "workspace.init", map[string]any{"cwd": cwd}, http.StatusOK)
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != 1 || report.Files[0].Status != "failed" || report.Files[0].Error == "" {
		t.Fatalf("init failure report=%+v", report)
	}
}
