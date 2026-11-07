package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestSessionForkAdditiveEchoAndLegacyPlainResult(t *testing.T) {
	var gotID, gotAnchor, gotTitle string
	s := newTestServer(t, Options{
		ForkAnchored: func(id string, anchor, title *string) (ForkResult, error) {
			gotID = id
			if anchor != nil {
				gotAnchor = *anchor
			}
			if title != nil {
				gotTitle = *title
			}
			return ForkResult{SessionID: "child", Anchor: anchor, Title: title}, nil
		},
	})
	rec := postJSON(t, s, "/api/session.fork", `{"id":"source","anchor":"17","title":"  exact  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("fork status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["sessionId"] != "child" || got["anchor"] != "17" || got["title"] != "  exact  " {
		t.Fatalf("additive result=%v", got)
	}
	if gotID != "source" || gotAnchor != "17" || gotTitle != "  exact  " {
		t.Fatalf("callback got id=%q anchor=%q title=%q", gotID, gotAnchor, gotTitle)
	}

	legacy := newTestServer(t, Options{Fork: func(id, title string) (string, error) { return "plain-child", nil }})
	rec = postJSON(t, legacy, "/api/session.fork", `{"id":"source"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain fork status=%d", rec.Code)
	}
	plain := unwrapResult(t, rec.Body.Bytes())
	if len(plain) != 1 || plain["sessionId"] != "plain-child" {
		t.Fatalf("plain old result changed: %v", plain)
	}
}

func TestSessionForkInvalidAnchorIsBadRequest(t *testing.T) {
	s := newTestServer(t, Options{ForkAnchored: func(string, *string, *string) (ForkResult, error) {
		return ForkResult{}, ErrInvalidAnchor
	}})
	rec := postJSON(t, s, "/api/session.fork", `{"id":"source","anchor":"assistant-seq"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid anchor status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRewindMethodsPreservePreviewAndPartialOutcome(t *testing.T) {
	s := newTestServer(t, Options{
		RewindPreview: func(id, anchor string) (RewindPreview, error) {
			if id != "source" || anchor != "9" {
				t.Fatalf("preview args=%q,%q", id, anchor)
			}
			return RewindPreview{FilesChanged: []string{"a.txt"}, Insertions: 2, Deletions: 1}, nil
		},
		RewindApply: func(id, anchor, mode string) (RewindResult, error) {
			if mode != "both" {
				t.Fatalf("mode=%q", mode)
			}
			return RewindResult{Kind: "rewound", Files: &RewindPreview{FilesChanged: []string{"a.txt"}}, ConversationError: "fork write failed"}, nil
		},
	})
	rec := postJSON(t, s, "/api/session.rewind.preview", `{"id":"source","anchor":"9"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", rec.Code, rec.Body.String())
	}
	preview := unwrapResult(t, rec.Body.Bytes())
	if preview["insertions"] != float64(2) || preview["deletions"] != float64(1) {
		t.Fatalf("preview=%v", preview)
	}
	rec = postJSON(t, s, "/api/session.rewind.apply", `{"id":"source","anchor":"9","mode":"both"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Result RewindResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Result.Kind != "rewound" || env.Result.SessionID != "" || env.Result.Files == nil || env.Result.ConversationError != "fork write failed" {
		t.Fatalf("partial outcome=%+v", env.Result)
	}
}

func TestRewindRefusalAndValidation(t *testing.T) {
	called := false
	s := newTestServer(t, Options{RewindApply: func(string, string, string) (RewindResult, error) {
		called = true
		return RewindResult{Kind: "refused", Reason: "anchor is not a user message"}, nil
	}})
	rec := postJSON(t, s, "/api/session.rewind.apply", `{"id":"source","anchor":"x","mode":"files"}`)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("refusal status=%d called=%v", rec.Code, called)
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["kind"] != "refused" || got["reason"] == "" {
		t.Fatalf("refusal=%v", got)
	}
	called = false
	rec = postJSON(t, s, "/api/session.rewind.apply", `{"id":"source","anchor":"x","mode":"bad"}`)
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("invalid mode status=%d called=%v", rec.Code, called)
	}
}

func TestTranscriptPageHTTPAndErrors(t *testing.T) {
	s := newTestServer(t, Options{TranscriptPage: func(id, cursor string, limit int) (TranscriptPageResult, error) {
		if id != "source" || cursor != "opaque" || limit != 2 {
			t.Fatalf("page args=%q,%q,%d", id, cursor, limit)
		}
		return TranscriptPageResult{Messages: []Message{}, HasOlder: false, NextCursor: "opaque"}, nil
	}})
	rec := postJSON(t, s, "/api/session.transcript.page", `{"id":"source","cursor":"opaque","limit":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := unwrapResult(t, rec.Body.Bytes())
	if got["hasOlder"] != false || got["nextCursor"] != "opaque" {
		t.Fatalf("page=%v", got)
	}
	if msgs, ok := got["messages"].([]any); !ok || len(msgs) != 0 {
		t.Fatalf("empty messages must be a real array: %v", got["messages"])
	}

	bad := newTestServer(t, Options{TranscriptPage: func(string, string, int) (TranscriptPageResult, error) {
		return TranscriptPageResult{}, ErrInvalidCursor
	}})
	if rec := postJSON(t, bad, "/api/session.transcript.page", `{"id":"source","cursor":"bad"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status=%d", rec.Code)
	}
	missing := newTestServer(t, Options{TranscriptPage: func(string, string, int) (TranscriptPageResult, error) {
		return TranscriptPageResult{}, ErrNoSuchSession
	}})
	if rec := postJSON(t, missing, "/api/session.transcript.page", `{"id":"missing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing session status=%d", rec.Code)
	}
}

func TestSessionOperationMethodsRemainOutsideCanonical25(t *testing.T) {
	if len(SessionMethods()) != 25 {
		t.Fatalf("canonical methods changed: %d", len(SessionMethods()))
	}
	for _, method := range SessionOperationMethods() {
		for _, canonical := range SessionMethods() {
			if method == canonical {
				t.Fatalf("additive method %s leaked into canonical", method)
			}
		}
	}
}

func TestSessionOperationMethodsUnwiredReturn501(t *testing.T) {
	s := newTestServer(t, Options{})
	for _, path := range []string{"/api/session.rewind.preview", "/api/session.rewind.apply", "/api/session.transcript.page"} {
		if rec := postJSON(t, s, path, `{"id":"s","anchor":"1"}`); rec.Code != http.StatusNotImplemented {
			t.Errorf("%s status=%d, want 501", path, rec.Code)
		}
	}
}

func TestTranscriptPageRejectsBadLimitBeforeCallback(t *testing.T) {
	called := false
	s := newTestServer(t, Options{TranscriptPage: func(string, string, int) (TranscriptPageResult, error) {
		called = true
		return TranscriptPageResult{}, errors.New("must not reach")
	}})
	for _, body := range []string{`{"id":"s","limit":-1}`, `{"id":"s","limit":201}`} {
		if rec := postJSON(t, s, "/api/session.transcript.page", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d", body, rec.Code)
		}
	}
	if called {
		t.Fatal("callback ran for invalid limit")
	}
}
