package transport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

type fakeAttachmentStore struct {
	putPath string
	getID   string
}

func (f *fakeAttachmentStore) Put(path string) (Attachment, error) {
	f.putPath = path
	return Attachment{ID: "att-real", Name: "pixel.png", ByteSize: 3, MIME: "image/png", SHA256: "abc", Width: 3, Height: 2}, nil
}

func (f *fakeAttachmentStore) Get(id string) (AttachmentContent, error) {
	f.getID = id
	if id != "att-real" {
		return AttachmentContent{}, errors.New("not found")
	}
	return AttachmentContent{Attachment: Attachment{ID: id, Name: "pixel.png", ByteSize: 3, MIME: "image/png", Width: 3, Height: 2}, Data: []byte{1, 2, 3}}, nil
}

func TestAttachmentPutGetAndOrderedPrompt(t *testing.T) {
	store := &fakeAttachmentStore{}
	var gotBlocks []ContentBlock
	legacyCalled := false
	s, err := NewServer(Options{
		Attachments: store,
		Prompt: func(string, string) (PromptResult, error) {
			legacyCalled = true
			return PromptResult{}, nil
		},
		PromptBlocks: func(sessionID string, blocks []ContentBlock) (PromptResult, error) {
			if sessionID != "session-a" {
				t.Fatalf("session = %q", sessionID)
			}
			gotBlocks = append([]ContentBlock(nil), blocks...)
			return PromptResult{Accepted: true, TurnID: "turn-blocks"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(base + "/connect?token=" + s.Token())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	post := func(method, body string) (int, map[string]json.RawMessage, []byte) {
		t.Helper()
		r, err := client.Post(base+"/api/"+method, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var env map[string]json.RawMessage
		_ = json.Unmarshal(raw, &env)
		return r.StatusCode, env, raw
	}

	status, env, raw := post("attachment.put", `{"path":"C:/explicit/pixel.png"}`)
	if status != http.StatusOK {
		t.Fatalf("attachment.put status=%d body=%s", status, raw)
	}
	var put struct {
		Attachment Attachment `json:"attachment"`
	}
	if err := json.Unmarshal(env["result"], &put); err != nil {
		t.Fatal(err)
	}
	if put.Attachment.ID != "att-real" || put.Attachment.Width != 3 || put.Attachment.Height != 2 || put.Attachment.ByteSize != 3 || store.putPath != "C:/explicit/pixel.png" {
		t.Fatalf("put metadata/store = %+v / %q", put.Attachment, store.putPath)
	}

	status, env, raw = post("attachment.get", `{"id":"att-real"}`)
	if status != http.StatusOK {
		t.Fatalf("attachment.get status=%d body=%s", status, raw)
	}
	var got struct {
		Attachment Attachment `json:"attachment"`
		DataBase64 string     `json:"dataBase64"`
	}
	if err := json.Unmarshal(env["result"], &got); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.DataBase64)
	if err != nil || !bytes.Equal(decoded, []byte{1, 2, 3}) || got.Attachment.ID != "att-real" || store.getID != "att-real" {
		t.Fatalf("get bytes/meta = %v %+v id=%q err=%v", decoded, got.Attachment, store.getID, err)
	}

	status, _, raw = post("session.prompt", `{"sessionId":"session-a","content":[{"type":"text","text":"before"},{"type":"attachment","attachmentId":"att-real"},{"type":"text","text":"after"}]}`)
	if status != http.StatusOK || legacyCalled {
		t.Fatalf("ordered prompt status=%d legacy=%v body=%s", status, legacyCalled, raw)
	}
	if len(gotBlocks) != 3 || gotBlocks[0].Text != "before" || gotBlocks[1].AttachmentID != "att-real" || gotBlocks[2].Text != "after" {
		t.Fatalf("ordered blocks = %+v", gotBlocks)
	}

	status, _, raw = post("session.prompt", `{"sessionId":"session-a","text":"legacy"}`)
	if status != http.StatusOK || !legacyCalled {
		t.Fatalf("legacy text must keep old callback: status=%d called=%v body=%s", status, legacyCalled, raw)
	}
}

func TestAttachmentRoutesRequireCookie(t *testing.T) {
	s, err := NewServer(Options{Attachments: &fakeAttachmentStore{}})
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	resp, err := http.Post(base+"/api/attachment.put", "application/json", strings.NewReader(`{"path":"C:/explicit/file"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized || len(body["error"]) != 4 {
		t.Fatalf("unauthorized status=%d error=%+v", resp.StatusCode, body["error"])
	}
}
