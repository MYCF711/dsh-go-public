package session

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/spi"
)

func anchoredForkSource(t *testing.T, home string) (*v4.Log, string, string) {
	t.Helper()
	l, err := CreateSession(home, t.TempDir(), "source-anchor", "standard")
	if err != nil {
		t.Fatal(err)
	}
	must := func(typ string, data any) int64 {
		t.Helper()
		if err := l.Append(typ, data); err != nil {
			t.Fatal(err)
		}
		return l.Seq() - 1
	}
	must("system/message", SystemMessageData(1, 1, "system"))
	must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "first"}}))
	must("assistant/message", AssistantMessageEvent(1, 1, []map[string]any{TextBlock("answer one")}, "stop", nil))
	second := must("user/message", UserMessage([]spi.Block{{Type: "text", Text: "second"}}))
	must("assistant/message", AssistantMessageEvent(2, 1, []map[string]any{TextBlock("answer two")}, "stop", nil))
	thirdData := UserMessage([]spi.Block{{Type: "text", Text: "third"}})
	must("user/message", thirdData)
	must("assistant/message", AssistantMessageEvent(3, 1, []map[string]any{TextBlock("answer three")}, "stop", nil))
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := jsonUnmarshal(thirdData, &struct {
		ID *string `json:"id"`
	}{ID: &id}); err != nil {
		t.Fatal(err)
	}
	return back, strconv.FormatInt(second, 10), id
}

func jsonUnmarshal(data any, out any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func TestForkAtAnchorIsInclusiveAndTitleSurvivesReopen(t *testing.T) {
	home := t.TempDir()
	src, secondSeq, _ := anchoredForkSource(t, home)
	title := "  Keep this title  "
	forked, err := ForkAt(src, home, src.Header.Cwd, "child-anchor", secondSeq, &title)
	if err != nil {
		t.Fatal(err)
	}
	if err := forked.Close(); err != nil {
		t.Fatal(err)
	}
	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatal(err)
	}
	nodes := Surface(back)
	var texts []string
	for _, n := range nodes {
		texts = append(texts, n.Message.Content)
	}
	want := []string{"system", "first", "answer one", "second"}
	if len(texts) != len(want) {
		t.Fatalf("anchored surface=%q, want %q", texts, want)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("anchored surface=%q, want %q", texts, want)
		}
	}
	foundTitle := false
	for _, ev := range back.Events {
		if ev.Type != "session/title" {
			continue
		}
		var d struct {
			Title       string  `json:"title"`
			MessageSeqs []int64 `json:"messageSeqs"`
			Source      struct {
				Kind string `json:"kind"`
			} `json:"source"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d.Title == title && d.MessageSeqs != nil && len(d.MessageSeqs) == 0 && d.Source.Kind == "user" {
			foundTitle = true
		}
	}
	if !foundTitle {
		t.Fatal("official session/title event was not persisted verbatim")
	}
	list, err := ListSessions(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		if row.SessionID == "child-anchor" {
			if row.Title == nil || *row.Title != title {
				t.Fatalf("reopened title=%v, want exact %q", row.Title, title)
			}
			return
		}
	}
	t.Fatal("forked child missing after reopen")
}

func TestForkAtAcceptsPersistentMessageIDAndRejectsBadAnchors(t *testing.T) {
	home := t.TempDir()
	src, secondSeq, thirdID := anchoredForkSource(t, home)
	forked, err := ForkAt(src, home, src.Header.Cwd, "child-id", thirdID, nil)
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()
	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(Surface(back)); got != 6 {
		t.Fatalf("persistent id anchored fork has %d surface nodes, want 6", got)
	}
	for _, bad := range []string{"", "999999", "not-an-anchor"} {
		if _, err := ForkAt(src, home, src.Header.Cwd, "bad-"+bad, bad, nil); !errors.Is(err, ErrInvalidAnchor) {
			t.Errorf("anchor %q error=%v, want ErrInvalidAnchor", bad, err)
		}
	}
	// An assistant seq is a real durable event but not a selectable user anchor.
	assistantSeq := strconv.FormatInt(src.Events[2].Seq, 10)
	if assistantSeq == secondSeq {
		t.Fatal("bad test fixture")
	}
	if _, err := ForkAt(src, home, src.Header.Cwd, "bad-assistant", assistantSeq, nil); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("assistant anchor error=%v, want ErrInvalidAnchor", err)
	}
	if _, err := os.Stat(forked.Path); err != nil {
		t.Fatal(err)
	}
}

func TestForkBeforeAnchorCutsConversationAndRefusesFirstUser(t *testing.T) {
	home := t.TempDir()
	src, secondSeq, _ := anchoredForkSource(t, home)
	forked, err := ForkBefore(src, home, src.Header.Cwd, "child-before", secondSeq)
	if err != nil {
		t.Fatal(err)
	}
	forked.Close()
	back, err := v4.ReadFile(forked.Path)
	if err != nil {
		t.Fatal(err)
	}
	nodes := Surface(back)
	if len(nodes) != 3 || nodes[2].Message.Content != "answer one" {
		t.Fatalf("before-anchor surface=%+v, want system/first/answer one", nodes)
	}
	firstSeq := strconv.FormatInt(src.Events[1].Seq, 10)
	if _, err := ForkBefore(src, home, src.Header.Cwd, "child-first", firstSeq); !errors.Is(err, ErrNoRewindConversation) {
		t.Fatalf("first user rewind error=%v, want ErrNoRewindConversation", err)
	}
}
