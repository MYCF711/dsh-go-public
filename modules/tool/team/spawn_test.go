package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 验收 spawn_teammate 的六道校验。任务书要求"每个 error 分支都带 Hint"，
// 所以每一条都同时断言"拒绝了"和"说清了怎么办"。
func TestSpawnTeammateContract(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		wantHint string // Hint 里必须出现的片段
	}{
		{"名字不合规", `{"name":"Bad_Name","description":"d","prompt":"p"}`, "小写"},
		{"名字太短", `{"name":"a","description":"d","prompt":"p"}`, "小写"},
		{"description 为空", `{"name":"ok","description":"  ","prompt":"p"}`, "负责"},
		{"description 超 120 字", `{"name":"ok","description":"` + strings.Repeat("字", 130) + `","prompt":"p"}`, "超"},
		{"prompt 为空", `{"name":"ok","description":"d","prompt":" "}`, "任务"},
		{"context 非法", `{"name":"ok","description":"d","prompt":"p","context":"clone"}`, "fresh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tool := &spawnTool{dir: t.TempDir()}
			res, err := tool.Exec(context.Background(), json.RawMessage(c.args))
			if err == nil {
				t.Fatalf("应当拒绝: %s", c.args)
			}
			if res.Hint == "" {
				t.Fatal("error 分支必须设 Hint（红线 R3）")
			}
			if !strings.Contains(res.Hint, c.wantHint) {
				t.Fatalf("Hint 要说清怎么办，期望包含 %q，实际 %q", c.wantHint, res.Hint)
			}
		})
	}
}

// 超长描述要算出**具体超了几个字**，不能只说"太长了"。
func TestSpawnReportsExactOverflow(t *testing.T) {
	tool := &spawnTool{dir: t.TempDir()}
	res, err := tool.Exec(context.Background(), json.RawMessage(`{"name":"ok","description":"`+strings.Repeat("字", 125)+`","prompt":"p"}`))
	if err == nil {
		t.Fatal("应当拒绝")
	}
	if !strings.Contains(res.Hint, "5") {
		t.Fatalf("Hint 要给出超了 5 个字: %q", res.Hint)
	}
}

// 成功路径：落盘、计数、created_at 是 RFC3339 且就是刚才。
//
// 曾经这里断言"不以 Z 结尾"，本意是"别把 UTC 当本地时间写出去"。
// 那条断言验的不是代码，是**跑测试的机器在哪个时区**：CI 的 runner 就是 UTC，
// `time.Now().Format(time.RFC3339)` 在 UTC 上合法地输出 `...Z`，于是断言在 CI 上恒红、
// 在开发机上恒绿——两边都不说明产品对不对。改成只验契约：能按 RFC3339 解析、
// 且与现在相差不到一分钟；时区偏移量本来就该由本机决定，不该由测试决定。
func TestSpawnPersistsAndCounts(t *testing.T) {
	dir := t.TempDir()
	tool := &spawnTool{dir: dir}
	for i, name := range []string{"alice", "bob"} {
		res, err := tool.Exec(context.Background(), json.RawMessage(`{"name":"`+name+`","description":"负责一件事","prompt":"开工"}`))
		if err != nil {
			t.Fatalf("%s: %v / %q", name, err, res.Hint)
		}
		m, _ := res.Data.(map[string]any)
		if m["total"] != i+1 {
			t.Fatalf("%s 之后 total = %v，期望 %d", name, m["total"], i+1)
		}
		created, _ := m["created_at"].(string)
		if created == "" {
			t.Fatalf("%s 的 created_at 是空的", name)
		}
		ts, err := time.Parse(time.RFC3339, created)
		if err != nil {
			t.Fatalf("%s 的 created_at 不是 RFC3339: %q（%v）", name, created, err)
		}
		if d := time.Since(ts); d > time.Minute || d < -time.Minute {
			t.Fatalf("%s 的 created_at 不是刚才: %q（差 %v）", name, created, d)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Agents []struct {
			Name      string `json:"name"`
			CreatedAt string `json:"created_at"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Agents) != 2 || f.Agents[0].Name != "alice" {
		t.Fatalf("落盘内容不对: %+v", f.Agents)
	}
	if f.Agents[0].CreatedAt == "" {
		t.Fatal("created_at 不能在落盘时丢掉")
	}
}

// 重名要拒绝，并且说清现在有谁。
func TestSpawnRejectsDuplicate(t *testing.T) {
	tool := &spawnTool{dir: t.TempDir()}
	ctx := context.Background()
	if _, err := tool.Exec(ctx, json.RawMessage(`{"name":"alice","description":"d","prompt":"p"}`)); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Exec(ctx, json.RawMessage(`{"name":"alice","description":"d","prompt":"p"}`))
	if err == nil {
		t.Fatal("重名必须拒绝")
	}
	if !strings.Contains(res.Hint, "alice") {
		t.Fatalf("Hint 要指出是哪个名字: %q", res.Hint)
	}
}

// 文件不存在 = 空名单，不是错误。
func TestSpawnToleratesMissingFile(t *testing.T) {
	tool := &spawnTool{dir: filepath.Join(t.TempDir(), "还没有这个目录")}
	if _, err := tool.Exec(context.Background(), json.RawMessage(`{"name":"alice","description":"d","prompt":"p"}`)); err != nil {
		t.Fatalf("目录不存在时应当自己建: %v", err)
	}
}
