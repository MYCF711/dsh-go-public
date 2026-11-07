package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestMetadataInitMethodsAreAdditiveAndUnwired501(t *testing.T) {
	if len(SessionMethods()) != 25 || len(MetadataInitMethods()) != 4 {
		t.Fatalf("canonical/additive methods=%d/%d", len(SessionMethods()), len(MetadataInitMethods()))
	}
	for _, method := range MetadataInitMethods() {
		for _, canonical := range SessionMethods() {
			if method == canonical {
				t.Fatalf("additive method %s leaked into canonical", method)
			}
		}
	}
	s := newTestServer(t, Options{})
	for _, method := range MetadataInitMethods() {
		if rec := postJSON(t, s, "/api/"+string(method), `{}`); rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status=%d body=%s", method, rec.Code, rec.Body.String())
		}
	}
}

type durableMetadataWitness struct {
	title *string
	color *string
}

func (w durableMetadataWitness) AppendJournalFrame(_ string, frame ServerFrame) error {
	switch frame.Type {
	case "session/title":
		var data struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal(frame.Data, &data); err != nil {
			return err
		}
		if w.title == nil || *w.title != data.Title {
			return errors.New("journal append ran before durable title write")
		}
	case "session/color":
		var data struct {
			Color string `json:"color"`
		}
		if err := json.Unmarshal(frame.Data, &data); err != nil {
			return err
		}
		if w.color == nil || *w.color != data.Color {
			return errors.New("journal append ran before durable color write")
		}
	}
	return nil
}
func (durableMetadataWitness) ResetJournal(string) error { return nil }

func TestSessionMetadataCommitFollowsDurableWriteAndUsesJournal(t *testing.T) {
	journals := NewJournals(nil)
	var durableTitle, durableColor string
	journals.SetPersistence(durableMetadataWitness{title: &durableTitle, color: &durableColor})
	s := newTestServer(t, Options{
		Journals: journals,
		RenameSession: func(id, title string, commit func() error) error {
			if id != "a" {
				t.Fatalf("rename id=%q", id)
			}
			durableTitle = title
			return commit()
		},
		SetSessionColor: func(id, color string, commit func() error) error {
			durableColor = color
			return commit()
		},
	})
	const title = "  exact title  "
	if rec := postJSON(t, s, "/api/session.rename", `{"id":"a","title":"  exact title  "}`); rec.Code != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", rec.Code, rec.Body.String())
	}
	if durableTitle != title {
		t.Fatalf("durable title=%q", durableTitle)
	}
	if rec := postJSON(t, s, "/api/session.color.set", `{"id":"a","color":"cyan"}`); rec.Code != http.StatusOK {
		t.Fatalf("color status=%d body=%s", rec.Code, rec.Body.String())
	}
	if durableColor != "cyan" {
		t.Fatalf("durable color=%q", durableColor)
	}
	win := journals.Get("a").Window(0, 0)
	if len(win.Frames) != 2 || win.Frames[0].Seq != 1 || win.Frames[1].Seq != 2 ||
		win.Frames[0].Type != "session/title" || win.Frames[1].Type != "session/color" {
		t.Fatalf("journal frames=%+v", win.Frames)
	}
	var titleData struct {
		Title       string  `json:"title"`
		MessageSeqs []int64 `json:"messageSeqs"`
		Source      struct {
			Kind string `json:"kind"`
		} `json:"source"`
	}
	if err := json.Unmarshal(win.Frames[0].Data, &titleData); err != nil {
		t.Fatal(err)
	}
	if titleData.Title != title || titleData.MessageSeqs == nil || titleData.Source.Kind != "user" {
		t.Fatalf("title data=%+v", titleData)
	}
	var colorData struct {
		Color string `json:"color"`
	}
	if err := json.Unmarshal(win.Frames[1].Data, &colorData); err != nil || colorData.Color != "cyan" {
		t.Fatalf("color data=%+v err=%v", colorData, err)
	}
}

type failingMetadataJournalPersistence struct{}

func (failingMetadataJournalPersistence) AppendJournalFrame(string, ServerFrame) error {
	return errors.New("journal write failed")
}
func (failingMetadataJournalPersistence) ResetJournal(string) error { return nil }

func TestSessionMetadataJournalFailureReturnsErrorWithoutFrame(t *testing.T) {
	journals := NewJournals(nil)
	journals.SetPersistence(failingMetadataJournalPersistence{})
	durable := false
	s := newTestServer(t, Options{
		Journals: journals,
		RenameSession: func(_, _ string, commit func() error) error {
			durable = true
			return commit()
		},
	})
	rec := postJSON(t, s, "/api/session.rename", `{"id":"a","title":"durable"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("journal failure status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !durable || journals.Get("a").LastSeq() != 0 {
		t.Fatalf("durable=%v lastSeq=%d", durable, journals.Get("a").LastSeq())
	}
	// 失败值不进入延迟补播队列；下一次成功只发布当时的最新值。
	journals.SetPersistence(nil)
	next := newTestServer(t, Options{Journals: journals, RenameSession: func(_, _ string, commit func() error) error {
		return commit()
	}})
	if rec := postJSON(t, next, "/api/session.rename", `{"id":"a","title":"next"}`); rec.Code != http.StatusOK {
		t.Fatalf("next mutation status=%d body=%s", rec.Code, rec.Body.String())
	}
	win := journals.Get("a").Window(0, 0)
	if len(win.Frames) != 1 {
		t.Fatalf("failed mutation was replayed unexpectedly: %+v", win.Frames)
	}
	var data struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(win.Frames[0].Data, &data); err != nil || data.Title != "next" {
		t.Fatalf("next frame=%+v err=%v", data, err)
	}
}

func TestSessionMetadataValidationAndWorkspaceInitReport(t *testing.T) {
	called := false
	s := newTestServer(t, Options{
		RenameSession: func(_, _ string, _ func() error) error { called = true; return nil },
		WorkspaceInit: func(cwd string) (WorkspaceInitResult, error) {
			if cwd != "/work" {
				t.Fatalf("cwd=%q", cwd)
			}
			return WorkspaceInitResult{Files: []WorkspaceInitFile{
				{Path: "/work/AGENTS.md", Status: "created"},
				{Path: "/work/docs/AGENTS.md", Status: "failed", Error: "permission denied"},
			}}, nil
		},
	})
	for _, body := range []string{`{"id":"a","title":""}`, `{"id":"a","title":" \n\t"}`, `{"title":"x"}`} {
		if rec := postJSON(t, s, "/api/session.rename", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d", body, rec.Code)
		}
	}
	if called {
		t.Fatal("invalid rename reached callback")
	}
	rec := postJSON(t, s, "/api/workspace.init", `{"cwd":"/work"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("init status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result WorkspaceInitResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Result.Files) != 2 || env.Result.Files[0].Status != "created" ||
		env.Result.Files[1].Status != "failed" || env.Result.Files[1].Error != "permission denied" {
		t.Fatalf("partial init report=%+v", env.Result)
	}
}

func TestWorkspaceInitRejectsFalseEmptyOrMalformedSuccess(t *testing.T) {
	for _, report := range []WorkspaceInitResult{
		{},
		{Files: []WorkspaceInitFile{}},
		{Files: []WorkspaceInitFile{{Path: "/work/AGENTS.md", Status: "written"}}},
		{Files: []WorkspaceInitFile{{Path: "/work/AGENTS.md", Status: "failed"}}},
		{Files: []WorkspaceInitFile{{Path: "/work/AGENTS.md", Status: "created", Error: "false"}}},
	} {
		s := newTestServer(t, Options{WorkspaceInit: func(string) (WorkspaceInitResult, error) { return report, nil }})
		if rec := postJSON(t, s, "/api/workspace.init", `{"cwd":"/work"}`); rec.Code != http.StatusInternalServerError {
			t.Errorf("report=%+v status=%d body=%s", report, rec.Code, rec.Body.String())
		}
	}
}
