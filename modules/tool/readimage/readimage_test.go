package readimage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePNG(t *testing.T, name string, w, h int, c color.Color) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 成功：必须把图片放进 Result.Images（否则模型看不见图，"读成功"是空话）。
func TestReturnsImageForModel(t *testing.T) {
	p := writePNG(t, "red.png", 4, 4, color.RGBA{255, 0, 0, 255})
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"file_path":"`+filepath.ToSlash(p)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("必须带一张图片，实际 %d", len(res.Images))
	}
	if res.Images[0].MIME != "image/png" {
		t.Fatalf("MIME 不对: %s", res.Images[0].MIME)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Images[0].DataBase64)
	if err != nil {
		t.Fatalf("base64 不合法: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("解回来不是 PNG: %v", err)
	}
	if d := res.Data.(data); d.MIME != "image/png" || d.Bytes == 0 {
		t.Fatalf("Data 不对: %+v", d)
	}
}

// 改过名的文本文件：按内容判类型，不信扩展名。
func TestRejectsNonImageDespiteExtension(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fake.png")
	os.WriteFile(p, []byte("this is not a png"), 0o644)
	tool := &Tool{}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"file_path":"`+filepath.ToSlash(p)+`"}`))
	if err == nil {
		t.Fatal("不是图片必须报错")
	}
	if !strings.Contains(res.Hint, "PNG/JPEG/WebP/GIF") {
		t.Fatalf("Hint 要说清支持什么: %q", res.Hint)
	}
}

// 空文件 / 目录 / 不存在：三种都要带下一步。
func TestBadTargets(t *testing.T) {
	tool := &Tool{}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"file_path":"nope.png"}`)); err == nil {
		t.Fatal("不存在要报错")
	}
	dir := t.TempDir()
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"file_path":"`+filepath.ToSlash(dir)+`"}`)); err == nil {
		t.Fatal("目录要报错")
	}
	empty := filepath.Join(t.TempDir(), "e.png")
	os.WriteFile(empty, nil, 0o644)
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"file_path":"`+filepath.ToSlash(empty)+`"}`))
	if err == nil || !strings.Contains(res.Hint, "空文件") {
		t.Fatalf("空文件要报错: %v %q", err, res.Hint)
	}
}
