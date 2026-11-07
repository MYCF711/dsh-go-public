package v4

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRoundTripGolden(t *testing.T) {
	src, err := ReadFile(filepath.Join("testdata", "small.v4.jsonl.zstd"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "rewritten.v4.jsonl.zstd")

	w, err := Create(out, src.Header)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(src.Events...); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(out)
	if err != nil {
		t.Fatalf("我们写出的文件自己都读不了: %v", err)
	}
	if !reflect.DeepEqual(got.Header, src.Header) {
		t.Fatalf("header 不一致:\n got %+v\nwant %+v", got.Header, src.Header)
	}
	if len(got.Events) != len(src.Events) {
		t.Fatalf("事件数 %d，期望 %d", len(got.Events), len(src.Events))
	}
	for i := range src.Events {
		if !bytes.Equal(got.Events[i].Raw, src.Events[i].Raw) {
			t.Fatalf("第 %d 条事件字节不一致:\n got %s\nwant %s", i, got.Events[i].Raw, src.Events[i].Raw)
		}
	}
}

func TestHeaderFrameIsExactlyOneLine(t *testing.T) {
	out := filepath.Join(t.TempDir(), "h.v4.jsonl.zstd")
	w, err := Create(out, Header{Type: "session", Version: 4, ID: "s1", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := framePlaintexts(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("帧数 = %d，期望 1", len(ps))
	}
	if !bytes.HasSuffix(ps[0], []byte("\n")) {
		t.Fatal("header 帧没有以 \\n 结尾")
	}
	if n := bytes.Count(ps[0], []byte("\n")); n != 1 {
		t.Fatalf("header 帧有 %d 行，契约要求恰好 1 行", n)
	}
}

func TestFramesCarryChecksum(t *testing.T) {
	out := filepath.Join(t.TempDir(), "c.v4.jsonl.zstd")
	w, err := Create(out, Header{Type: "session", Version: 4, ID: "s1", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{Type: "turn/start", Seq: 0, Time: 2, Data: []byte(`{"turn":1}`)}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	frames, _, err := splitFrames(b)
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range frames {
		fhd := b[f.offset+4]
		if fhd&0x04 == 0 {
			t.Fatalf("第 %d 帧没有开 checksum（FHD=0x%02x）", i, fhd)
		}
	}
}

func TestAppendOnlyNeverRewrites(t *testing.T) {
	out := filepath.Join(t.TempDir(), "a.v4.jsonl.zstd")
	w, err := Create(out, Header{Type: "session", Version: 4, ID: "s1", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{Type: "turn/start", Seq: 0, Time: 2, Data: []byte(`{"turn":1}`)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{Type: "turn/end", Seq: 1, Time: 3, Data: []byte(`{"turn":1,"reason":{"kind":"completed"}}`)}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Fatal("追加动了已提交的字节：append-only 被破坏")
	}
	if len(after) <= len(before) {
		t.Fatal("追加之后文件没有变长")
	}
}

func TestOpenAppendRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.v4.jsonl.zstd")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAppend(path); err == nil {
		t.Fatal("损坏的文件必须拒绝追加")
	}
}

func TestOpenAppendContinuesFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cont.v4.jsonl.zstd")
	w, err := Create(out, Header{Type: "session", Version: 4, ID: "s1", CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(Event{Type: "turn/start", Seq: 0, Time: 2, Data: []byte(`{"turn":1}`)}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	w2, err := OpenAppend(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Append(Event{Type: "turn/end", Seq: 1, Time: 3, Data: []byte(`{"turn":1,"reason":{"kind":"completed"}}`)}); err != nil {
		t.Fatal(err)
	}
	w2.Close()

	l, err := ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Events) != 2 {
		t.Fatalf("事件数 = %d，期望 2", len(l.Events))
	}
	if l.Header.ID != "s1" {
		t.Fatalf("追加把 header 弄丢了: %+v", l.Header)
	}
}

func TestHeaderMarshalFieldOrder(t *testing.T) {
	h := Header{Type: "session", Version: 4, ID: "s", CreatedAt: 7, Cwd: `D:\x`, AgentPreset: "standard"}
	b, _ := h.MarshalJSON()
	want := `{"type":"session","version":4,"id":"s","createdAt":7,"cwd":"D:\\x","isSeeded":false,"delegationDepth":0,"agentPreset":"standard"}`
	if string(b) != want {
		t.Fatalf("header 字节:\n got %s\nwant %s", b, want)
	}
}

// TestRoundTripRealCorpus 是 P0 判据③的 Go 侧证据：真实历史读进来、写出去、再读回来，
// 事件字节必须逐条相同。
func TestRoundTripRealCorpus(t *testing.T) {
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessions := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("本机没有真实会话目录：", sessions)
	}
	dir := t.TempDir()
	var checked int
	var failures []string

	err := filepath.Walk(sessions, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") || checked >= 12 {
			return nil
		}
		src, err := ReadFile(p)
		if err != nil {
			failures = append(failures, p+": 读失败 "+err.Error())
			return nil
		}
		out := filepath.Join(dir, "rt-"+filepath.Base(filepath.Dir(p))+".zstd")
		w, err := Create(out, src.Header)
		if err != nil {
			failures = append(failures, p+": 建文件失败 "+err.Error())
			return nil
		}
		if err := w.Append(src.Events...); err != nil {
			failures = append(failures, p+": 写失败 "+err.Error())
			return nil
		}
		if err := w.Close(); err != nil {
			failures = append(failures, p+": 关闭失败 "+err.Error())
			return nil
		}
		got, err := ReadFile(out)
		if err != nil {
			failures = append(failures, p+": 读回失败 "+err.Error())
			return nil
		}
		if !reflect.DeepEqual(got.Header, src.Header) {
			failures = append(failures, p+": header 变了")
		}
		if len(got.Events) != len(src.Events) {
			failures = append(failures, p+": 事件数变了")
		}
		for i := range src.Events {
			if i < len(got.Events) && !bytes.Equal(got.Events[i].Raw, src.Events[i].Raw) {
				failures = append(failures, p+": 第 "+string(rune('0'+i%10))+" 条事件字节变了")
				break
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("往返校验：%d 个真实会话", checked)
	if len(failures) > 0 {
		t.Fatalf("%d 个失败：\n%s", len(failures), strings.Join(failures, "\n"))
	}
}
