package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 换一个 cwd 之后还能按 id 找到会话。
//
// projectKey 是按 cwd 字符串算的：同一个逻辑 workspace 换个盘符/挂载点/另一台机器，
// 目录名就变了，而 session id 是全局唯一的。实测踩过——
// 拿正确的 id + 换过的 cwd 去 resume，报「找不到会话」。
func TestResolvePathFallsBackToID(t *testing.T) {
	home := t.TempDir()
	oldCwd := t.TempDir()
	newCwd := t.TempDir()
	id := NewID()

	lg, err := CreateSession(home, oldCwd, id, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lg.Append("user/message", map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}

	// 同一个 cwd：算出来的位置就是它
	direct, err := ResolvePath(home, oldCwd, id)
	if err != nil {
		t.Fatalf("同一个 cwd 应该直接找到: %v", err)
	}
	if filepath.Base(filepath.Dir(direct)) != id {
		t.Fatalf("路径不对: %s", direct)
	}

	// 换了 cwd：必须回退到按 id 找
	fallback, err := ResolvePath(home, newCwd, id)
	if err != nil {
		t.Fatalf("换 cwd 之后应该能按 id 找到: %v", err)
	}
	if fallback != direct {
		t.Fatalf("应该指向同一份文件：%s vs %s", fallback, direct)
	}
	if _, err := os.Stat(fallback); err != nil {
		t.Fatalf("回退后的路径要真实存在: %v", err)
	}
}

// 真的没有这份会话时，要说清试过哪两个地方。
func TestResolvePathMissingMentionsBoth(t *testing.T) {
	home := t.TempDir()
	_, err := ResolvePath(home, t.TempDir(), "session-nope")
	if err == nil {
		t.Fatal("不存在的会话必须报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "按 cwd 算出的位置没有") || !strings.Contains(msg, "按 id 也没找到") {
		t.Fatalf("要说清试过哪两条路: %v", err)
	}
}
