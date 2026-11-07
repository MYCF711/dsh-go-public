package approval

import (
	"sync"
	"testing"
)

func TestRememberAndCheck(t *testing.T) {
	m := New()
	if m.Allowed("s1", "shell") {
		t.Fatal("没记过不该放行")
	}
	m.Remember("s1", "shell")
	if !m.Allowed("s1", "shell") {
		t.Fatal("记过之后应该放行")
	}
	if !m.Allowed("s1", "shell") {
		t.Fatal("重复查询不该失效")
	}
}

func TestScopedBySessionAndTool(t *testing.T) {
	m := New()
	m.Remember("s1", "shell")
	if m.Allowed("s2", "shell") {
		t.Fatal("别的会话不该继承批准")
	}
	if m.Allowed("s1", "write") {
		t.Fatal("批准 shell 不等于批准 write")
	}
}

func TestForget(t *testing.T) {
	m := New()
	m.Remember("s1", "shell")
	m.Forget("s1")
	if m.Allowed("s1", "shell") {
		t.Fatal("Forget 之后必须重新问")
	}
}

func TestSnapshot(t *testing.T) {
	m := New()
	m.Remember("s1", "shell")
	m.Remember("s1", "write")
	got := m.Snapshot("s1")
	if len(got) != 2 {
		t.Fatalf("快照 = %v，期望 2 个", got)
	}
	if len(m.Snapshot("s2")) != 0 {
		t.Fatal("别的会话应该是空的")
	}
}

func TestConcurrentUse(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); m.Remember("s", "shell") }()
		go func() { defer wg.Done(); _ = m.Allowed("s", "shell") }()
	}
	wg.Wait()
	if !m.Allowed("s", "shell") {
		t.Fatal("并发写入后应该仍在")
	}
}
