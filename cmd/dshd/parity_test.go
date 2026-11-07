package main

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"dshgo/codecs/v4"
	"os"
)

// dshTool 是 DSH 真实会话里 `request/header.header.tools[]` 的形状。
type dshTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// dshToolsFixture 是与真实会话解耦的固定夹具（内容就是 tools 数组本身）。
//
// 为什么必须有它：会话目录每天都在新增，而 `request/header` 只在会话开头出现，
// 所以「扫前 60 个会话」抽出来的工具表会随时间的推移而变——同一条测试在不同时刻
// 给出不同答案，曾经造成「check.sh 绿、手动 go test 红」。夹具落盘后输入就是确定的，
// 只有显式 PARITY_REFRESH=1 才会重新采样。
const dshToolsFixture = "testdata/dsh-tools.json"

// loadDSHTools 返回「DSH 发给模型的那张工具表」。
//
// 默认读夹具；PARITY_REFRESH=1 时改为扫真实会话并覆盖写回夹具。
func loadDSHTools(t *testing.T) map[string]dshTool {
	t.Helper()

	if os.Getenv("PARITY_REFRESH") == "1" {
		tools := scanDSHTools(t)
		writeDSHToolsFixture(t, tools)
		t.Logf("PARITY_REFRESH=1：已用真实会话覆盖 %s，共 %d 个工具", dshToolsFixture, len(tools))
		return indexTools(tools)
	}

	raw, err := os.ReadFile(dshToolsFixture)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("没有 dsh-tools.json；设 PARITY_REFRESH=1 生成一次")
		}
		t.Fatalf("读夹具 %s 失败：%v", dshToolsFixture, err)
	}
	var tools []dshTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("夹具 %s 不是合法的工具数组：%v", dshToolsFixture, err)
	}
	if len(tools) == 0 {
		t.Fatalf("夹具 %s 是空的；设 PARITY_REFRESH=1 重新生成", dshToolsFixture)
	}
	return indexTools(tools)
}

// indexTools 按名字建索引；同名取第一个，空名忽略。
func indexTools(tools []dshTool) map[string]dshTool {
	out := make(map[string]dshTool, len(tools))
	for _, tl := range tools {
		if tl.Name == "" {
			continue
		}
		if _, ok := out[tl.Name]; !ok {
			out[tl.Name] = tl
		}
	}
	return out
}

// writeDSHToolsFixture 把采样结果覆盖写到夹具。
func writeDSHToolsFixture(t *testing.T, tools []dshTool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dshToolsFixture), 0o755); err != nil {
		t.Fatalf("建 testdata 目录失败：%v", err)
	}
	for i := range tools {
		if len(tools[i].Parameters) == 0 {
			tools[i].Parameters = json.RawMessage("null")
		}
	}
	buf, err := json.MarshalIndent(tools, "", "  ")
	if err != nil {
		t.Fatalf("序列化夹具失败：%v", err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(dshToolsFixture, buf, 0o644); err != nil {
		t.Fatalf("写夹具失败：%v", err)
	}
}

// scanDSHTools 从真实会话里抽出工具表并去重排序。
//
// 这是对拍的地基：`request/header.header.tools` 就是模型看到的东西，
// 不是我们从文档猜的。只在 PARITY_REFRESH=1 时被调用。
func scanDSHTools(t *testing.T) []dshTool {
	t.Helper()
	root := os.Getenv("DSH_HOME")
	if root == "" {
		root = `D:\DeepSeek Harness\home`
	}
	sessions := filepath.Join(root, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("本机没有真实会话目录")
	}
	out := map[string]dshTool{}
	err := filepath.Walk(sessions, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".zstd") || len(out) >= 60 {
			return nil
		}
		raw, err := v4.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, ev := range raw.Events {
			if ev.Type != "request/header" {
				continue
			}
			var d struct {
				Header struct {
					Tools []dshTool `json:"tools"`
				} `json:"header"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			for _, tl := range d.Header.Tools {
				if tl.Name != "" {
					if _, ok := out[tl.Name]; !ok {
						out[tl.Name] = tl
					}
				}
			}
			break
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Skip("没有找到带 request/header 的会话")
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names) // 夹具必须稳定：字典序，否则每次 diff 都在抖
	tools := make([]dshTool, 0, len(names))
	for _, name := range names {
		tools = append(tools, out[name])
	}
	return tools
}

type schema struct {
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required"`
}

func parseSchema(raw json.RawMessage) schema {
	var s schema
	_ = json.Unmarshal(raw, &s)
	return s
}

// coreTools 是"必须和 DSH 同名"的那 9 个：它们是 P0 判据点名的工具。
// 其它工具（比如 history_search）是我们的追加能力，DSH 里没有属正常。
var coreTools = map[string]bool{
	"read": true, "write": true, "edit": true, "glob": true, "grep": true,
	"bash": true, "job_output": true, "job_kill": true, "job_list": true,
}

// TestToolParityWithDSH 是「工具语义对拍」的门。
//
// 判定标准刻意窄，只拦**会导致模型调用失败**的差异：
//  1. 我们有的工具，DSH 里必须有同名工具（否则按 DSH 习惯写的调用会打空）；
//  2. DSH 标为 required 的参数，我们的 schema 必须认（否则模型照 DSH 传参会被我们拒）。
//
// 描述措辞、非必需参数的多少只报告，不判失败。
func TestToolParityWithDSH(t *testing.T) {
	dsh := loadDSHTools(t)
	ours := buildToolTableMust(t)

	var problems []string
	report := make([]string, 0, len(ours))

	for _, o := range ours {
		d, ok := dsh[o.Name]
		if !ok {
			// DSH 没有这个工具：我们独有的扩展（如 history_search）。
			// 加法不破坏 DSH 的调用契约，只报告；但核心 9 个必须同名，否则模型永远调不到。
			if coreTools[o.Name] {
				problems = append(problems, o.Name+": DSH 里没有同名工具（核心工具必须同名）")
			}
			report = append(report, o.Name+"\tDSH 无此工具")
			continue
		}
		var oursSchema, dshSchema schema
		_ = json.Unmarshal(o.Parameters, &oursSchema)
		dshSchema = parseSchema(d.Parameters)

		missing := []string{}
		for _, req := range dshSchema.Required {
			if _, ok := oursSchema.Properties[req]; !ok {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			problems = append(problems,
				o.Name+": DSH 要求但我们不认的参数 "+strings.Join(missing, ","))
		}

		extra := []string{}
		for p := range oursSchema.Properties {
			if _, ok := dshSchema.Properties[p]; !ok {
				extra = append(extra, p)
			}
		}
		sort.Strings(extra)
		status := "一致"
		if len(missing) > 0 {
			status = "缺参"
		} else if len(extra) > 0 {
			status = "超集"
		}
		report = append(report, o.Name+"\t"+status+"\t额外: "+strings.Join(extra, ","))
	}

	for _, line := range report {
		t.Logf("%s", line)
	}
	if len(problems) > 0 {
		t.Fatalf("与 DSH 的工具契约不一致：\n  %s", strings.Join(problems, "\n  "))
	}
}
