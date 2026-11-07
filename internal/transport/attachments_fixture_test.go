package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestAttachmentAdditiveSnapshotAndGolden(t *testing.T) {
	var snap struct {
		Canonical int                        `json:"canonicalMethodsUnchanged"`
		Methods   map[string]json.RawMessage `json:"methods"`
	}
	raw, err := os.ReadFile("testdata/attachments.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Canonical != 25 || len(snap.Methods) != 2 || len(AttachmentMethods()) != 2 {
		t.Fatalf("additive snapshot mismatch: %+v", snap)
	}
	for _, method := range AttachmentMethods() {
		if _, ok := snap.Methods[string(method)]; !ok {
			t.Errorf("snapshot missing %s", method)
		}
	}
	var golden struct {
		Attachment Attachment     `json:"attachment"`
		DataBase64 string         `json:"dataBase64"`
		Content    []ContentBlock `json:"content"`
	}
	raw, err = os.ReadFile("testdata/attachments-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(golden.DataBase64)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != golden.Attachment.ByteSize || hex.EncodeToString(digest[:]) != golden.Attachment.SHA256 || len(golden.Content) != 5 {
		t.Fatalf("golden bytes/content mismatch: %+v", golden)
	}
}
