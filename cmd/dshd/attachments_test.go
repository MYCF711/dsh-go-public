package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

func writeImageFixture(t *testing.T, path, kind string, width, height int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var out bytes.Buffer
	var err error
	switch kind {
	case "png":
		err = png.Encode(&out, img)
	case "jpeg":
		err = jpeg.Encode(&out, img, nil)
	case "gif":
		err = gif.Encode(&out, img, nil)
	default:
		t.Fatalf("unknown fixture %s", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentStoreRealBytesDimensionsAndIsolation(t *testing.T) {
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "home-a"), filepath.Join(root, "home-b")
	storeA, storeB := newAttachmentStore(homeA), newAttachmentStore(homeB)
	cases := []struct {
		name, kind, mime string
		width, height    int
	}{
		{"small.png", "png", "image/png", 3, 2},
		{"small.jpg", "jpeg", "image/jpeg", 4, 3},
		{"small.gif", "gif", "image/gif", 5, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			writeImageFixture(t, path, tc.kind, tc.width, tc.height)
			put, err := storeA.Put(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(put.ID, "att-") || put.Name != tc.name || put.ByteSize <= 0 || put.MIME != tc.mime || put.Width != tc.width || put.Height != tc.height || len(put.SHA256) != 64 {
				t.Fatalf("bad metadata: %+v", put)
			}
			got, err := storeA.Get(put.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, _ := os.ReadFile(path)
			if !bytes.Equal(got.Data, source) || got.Attachment.ByteSize != int64(len(source)) {
				t.Fatalf("stored bytes differ: got=%d source=%d", len(got.Data), len(source))
			}
			if _, err := storeB.Get(put.ID); err == nil {
				t.Fatal("A attachment must not be readable from B home")
			}
		})
	}

	webpPath := filepath.Join(root, "small.webp")
	webp, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webpPath, webp, 0o600); err != nil {
		t.Fatal(err)
	}
	webpPut, err := storeA.Put(webpPath)
	if err != nil {
		t.Fatal(err)
	}
	if webpPut.MIME != "image/webp" || webpPut.Width != 1 || webpPut.Height != 1 || webpPut.ByteSize != int64(len(webp)) {
		t.Fatalf("webp metadata = %+v", webpPut)
	}

	textPath := filepath.Join(root, "note.txt")
	text := []byte("真实文本附件\n第二行\n")
	if err := os.WriteFile(textPath, text, 0o600); err != nil {
		t.Fatal(err)
	}
	textPut, err := storeA.Put(textPath)
	if err != nil {
		t.Fatal(err)
	}
	if textPut.MIME != "text/plain" || textPut.Width != 0 || textPut.Height != 0 || textPut.ByteSize != int64(len(text)) {
		t.Fatalf("text metadata = %+v", textPut)
	}
	textGot, err := storeA.Get(textPut.ID)
	if err != nil || !bytes.Equal(textGot.Data, text) {
		t.Fatalf("text get = %q, %v", textGot.Data, err)
	}
}

func TestAttachmentStoreFailureBranchesAreReal(t *testing.T) {
	root := t.TempDir()
	store := newAttachmentStore(filepath.Join(root, "home"))
	if _, err := store.Put("relative.txt"); err == nil {
		t.Fatal("relative path must be rejected")
	}
	if _, err := store.Put(root); err == nil {
		t.Fatal("directory must be rejected")
	}
	if _, err := store.Put(filepath.Join(root, "missing.txt")); err == nil {
		t.Fatal("missing file must be rejected")
	}
	empty := filepath.Join(root, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(empty); err == nil {
		t.Fatal("zero bytes must be rejected")
	}
	unsupported := filepath.Join(root, "blob.bin")
	if err := os.WriteFile(unsupported, []byte{0, 1, 2, 3, 4}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(unsupported); err == nil {
		t.Fatal("unsupported bytes must be rejected")
	}
	bad := filepath.Join(root, "bad.png")
	if err := os.WriteFile(bad, append([]byte("\x89PNG\r\n\x1a\n"), []byte("not-an-image")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(bad); err == nil {
		t.Fatal("bad image must be rejected")
	}
	oversize := filepath.Join(root, "oversize.txt")
	f, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(attachmentMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(oversize); err == nil {
		t.Fatal("oversize attachment must be rejected")
	}
	if _, err := store.Get("../escape"); err == nil {
		t.Fatal("attachment id traversal must be rejected")
	}
	if _, err := store.Get("att-does-not-exist"); err == nil {
		t.Fatal("unknown attachment must be rejected")
	}
}

func TestResolveContentBlocksReferencesAndDurableOrder(t *testing.T) {
	home := t.TempDir()
	store := newAttachmentStore(home)
	cwd := t.TempDir()
	target, err := session.CreateSession(home, cwd, "session-target", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(cwd, "pixel.png")
	writeImageFixture(t, imagePath, "png", 3, 2)
	put, err := store.Put(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := store.ResolveBlocks([]transport.ContentBlock{
		{Type: "text", Text: "before"},
		{Type: "attachment", AttachmentID: put.ID, Name: "spoof.png", ByteSize: 1},
		{Type: "reference", ReferenceType: "file", Path: imagePath},
		{Type: "reference", ReferenceType: "session", SessionID: "session-target"},
		{Type: "text", Text: "after"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 || blocks[1].AttachmentID != put.ID || blocks[1].Name != "pixel.png" || blocks[1].ByteSize != put.ByteSize || blocks[1].Width != 3 || blocks[1].Height != 2 {
		t.Fatalf("resolved attachment spoof/order = %+v", blocks)
	}
	if blocks[2].ReferenceID == "" || blocks[2].ReferenceType != "file" || blocks[2].Name != "pixel.png" || blocks[3].ReferenceID == "" || blocks[3].SessionID != "session-target" {
		t.Fatalf("references = %+v", blocks[2:4])
	}
	if _, err := store.ResolveBlocks([]transport.ContentBlock{{Type: "attachment", AttachmentID: "att-missing"}}); err == nil {
		t.Fatal("missing attachment must fail")
	}
	if _, err := store.ResolveBlocks([]transport.ContentBlock{{Type: "reference", ReferenceType: "file", Path: filepath.Join(cwd, "missing")}}); err == nil {
		t.Fatal("missing file reference must fail")
	}
	if _, err := store.ResolveBlocks([]transport.ContentBlock{{Type: "reference", ReferenceType: "session", SessionID: "session-title-is-not-id"}}); err == nil {
		t.Fatal("missing session reference must fail")
	}

	logw, err := session.OpenSession(home, cwd, "session-target")
	if err != nil {
		t.Fatal(err)
	}
	if err := logw.Append("user/message", session.UserContentMessage(sessionBlocksFromTransport(blocks))); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}
	src := newSessionSource(home, "")
	msgs, err := src.Messages("session-target")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != 5 {
		t.Fatalf("load messages/content = %+v", msgs)
	}
	if msgs[0].Text != "before\nafter" || msgs[0].Content[1].AttachmentID != put.ID || msgs[0].Content[2].ReferenceID != blocks[2].ReferenceID || msgs[0].Content[3].ReferenceID != blocks[3].ReferenceID {
		t.Fatalf("durable order/identity = %+v", msgs[0])
	}
	path, err := src.resolve("session-target")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || !strings.Contains(string(raw.Events[0].Data), `"attachmentId":"`+put.ID+`"`) {
		t.Fatalf("v4 did not retain attachment block: %+v", raw.Events)
	}
}
