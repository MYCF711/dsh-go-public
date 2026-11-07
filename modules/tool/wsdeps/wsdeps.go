// Package wsdeps 提供 `load_workspace_dependencies` 工具：报告这台机器上跑
// Python/Office 活儿要用的运行时与库在哪、什么版本。
//
// 为什么需要它：模型要做 .xlsx/.docx 那类活儿时，先得知道有没有 openpyxl、
// python-docx，以及 python 在哪个路径。以前只能靠 bash 里试 `python -c "import ..."`，
// 一次一个库——既慢又容易在 PATH 上猜错。
//
// 这个工具**会起子进程**（两次 python 调用：一次取解释器版本，一次取库版本），
// 所以 Caps 里写了 Executes: true。按 AGENTS §2，审批与沙箱不按工具名猜——
// 起了进程就说起了进程。代价是 deny 策略下它会被拒，那是正确行为。
//
// 为什么是两趟而不是一趟（M290 回归的根因）：这台机器上 `python -c pass` 冷启动
// 就要 0.7~2.3 秒，再把 numpy/pandas/openpyxl/python-docx/python-pptx/Pillow/lxml
// 全导入一遍，实测 1.5~24 秒，并发重时更久。原来的实现把「python 能不能跑」绑在
// 这趟重库导入上，10 秒预算一到子进程被掐断——而 Windows 上 exec 只回
// "exit status 1"（不是 context.DeadlineExceeded），代码又把这个错误吞了。
// 于是 python 明明在，结果却是 {"path": "...python.exe", "ok": false}，还查不出原因。
// 现在 ok 只由**便宜的版本探针**决定；库版本是第二趟 best-effort：
// 查不完就说查不完（libraries_error），不谎报「没装」（missing）。
package wsdeps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

// 每趟子进程的预算。分开给的唯一理由：**慢的一趟不许把快的两趟一起拖垮**。
// 原来三件事共用一个 10 秒 ctx，python 一慢，node/pnpm 的版本也跟着空。
const (
	// versionBudget 只 import sys/json，正常 ~1 秒。给冷启动、杀软扫描、机器并发
	// 留 20 倍余量——「python 在不在」这种判据不该被负载抖没。
	versionBudget = 20 * time.Second
	// libraryBudget 要导入 7 个重库，实测 1.5~24 秒。超时只代表「没查完」，
	// 不影响 ok，也不许写成 missing。
	libraryBudget = 30 * time.Second
	// node --version 正常几十毫秒。
	nodeBudget = 10 * time.Second
	// pnpm 是 .cmd 批处理，Windows 上起得比 node 慢。
	pnpmBudget = 20 * time.Second
)

// versionScript 只取解释器版本——**不导入任何第三方库**。
// 「python 可用不可用」必须由它决定：重库导入慢是环境问题，不是 python 不存在。
const versionScript = `import json, sys
print(json.dumps({"version": "%d.%d.%d" % sys.version_info[:3]}))`

// librariesScript 一次取完所有库的版本。
//
// 关键点：**一个进程取全部**。七个库各起一个 python，在冷启动慢的机器上要几十秒，
// 而这里只需要一次导入。
const librariesScript = `import json
libs = {}
for mod, name in [("numpy","numpy"),("pandas","pandas"),("openpyxl","openpyxl"),
                  ("docx","python-docx"),("pptx","python-pptx"),
                  ("PIL","Pillow"),("lxml","lxml")]:
    try:
        m = __import__(mod)
        libs[name] = getattr(m, "__version__", None) or "unknown"
    except Exception:
        pass
print(json.dumps(libs))`

// libraryNames 是**安装名**（不是 import 名）——模型照着它 pip install 才装得对。
var libraryNames = []string{"numpy", "pandas", "openpyxl", "python-docx", "python-pptx", "Pillow", "lxml"}

type Tool struct{}

func (t *Tool) Name() string { return "load_workspace_dependencies" }
func (t *Tool) Caps() spi.Caps {
	// 起 python 子进程 = 执行。不因为"只是读版本"就把它藏起来。
	return spi.Caps{Executes: true}
}
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "查 Python/Node/pnpm 路径与 numpy/pandas/openpyxl/docx/pptx/Pillow/lxml 版本（会起子进程，deny 策略下会被拒）"
}

// deps 是返回给模型的结构化形状。
type deps struct {
	Python struct {
		Path    string `json:"path,omitempty"`
		Version string `json:"version,omitempty"`
		OK      bool   `json:"ok"`
		// Error 是探针失败的原因（预算用尽 / 非零退出 / stderr 摘要）。
		// 没有它，ok:false 就没法区分「没装」和「装了跑不起来」。
		Error string `json:"error,omitempty"`
	} `json:"python"`
	Libraries map[string]string `json:"libraries"`
	// LibrariesError 非空表示**库版本没查完**，不是「没装」。
	// 这两件事混在一起，模型会去重装一堆本来就有的包。
	LibrariesError string `json:"libraries_error,omitempty"`
	Node           struct {
		Path    string `json:"path,omitempty"`
		Version string `json:"version,omitempty"`
	} `json:"node"`
	Pnpm struct {
		Path    string `json:"path,omitempty"`
		Version string `json:"version,omitempty"`
	} `json:"pnpm"`
	Missing []string `json:"missing,omitempty"`
}

func (t *Tool) Exec(ctx context.Context, _ json.RawMessage) (spi.Result, error) {
	var d deps
	d.Libraries = map[string]string{}

	// Python：路径 + 版本 + 库，两趟取完。
	if pyPath, err := exec.LookPath("python"); err == nil {
		d.Python.Path = pyPath

		// 第一趟：便宜，决定 ok。只取解释器版本。
		if out, err := run(ctx, versionBudget, pyPath, "-c", versionScript); err != nil {
			d.Python.Error = err.Error()
		} else {
			var got struct {
				Version string `json:"version"`
			}
			if json.Unmarshal([]byte(out), &got) == nil && got.Version != "" {
				d.Python.Version = got.Version
				d.Python.OK = true
			} else {
				d.Python.Error = fmt.Sprintf("版本探针输出无法解析：%q", oneLine(out, 200))
			}
		}

		// 第二趟：贵，best-effort。查不完只记 LibrariesError，不改 ok。
		if d.Python.OK {
			if out, err := run(ctx, libraryBudget, pyPath, "-c", librariesScript); err != nil {
				d.LibrariesError = err.Error()
			} else {
				var libs map[string]string
				if json.Unmarshal([]byte(out), &libs) == nil {
					for k, v := range libs {
						d.Libraries[k] = v
					}
					for _, name := range libraryNames {
						if _, ok := d.Libraries[name]; !ok {
							d.Missing = append(d.Missing, name)
						}
					}
				} else {
					d.LibrariesError = fmt.Sprintf("库探针输出无法解析：%q", oneLine(out, 200))
				}
			}
		}
	} else {
		d.Missing = append(d.Missing, "python")
	}

	// Node / pnpm 只报路径与版本；没有就说没有，不编。
	if p, err := exec.LookPath("node"); err == nil {
		d.Node.Path = p
		if out, err := run(ctx, nodeBudget, p, "--version"); err == nil {
			d.Node.Version = trimNL(out)
		}
	} else {
		d.Missing = append(d.Missing, "node")
	}
	if p, err := exec.LookPath("pnpm"); err == nil {
		d.Pnpm.Path = p
		if out, err := run(ctx, pnpmBudget, p, "--version"); err == nil {
			d.Pnpm.Version = trimNL(out)
		}
	} else {
		d.Missing = append(d.Missing, "pnpm")
	}

	data, _ := json.MarshalIndent(d, "", "  ")

	res := spi.Result{
		Summary: fmt.Sprintf("python %s｜库 %d 个｜node %s｜pnpm %s",
			orNA(d.Python.Version), len(d.Libraries), orNA(d.Node.Version), orNA(d.Pnpm.Version)),
		Text: string(data),
		Data: d,
	}
	// 失败必须带 Hint（R3）：只给 ok:false 不给原因，模型和人都没法往下走。
	if d.Python.Error != "" {
		res.Hint = fmt.Sprintf(`python 在 %s，但探针没跑通（%s）；先用 bash 手工跑 `+
			"`python -c \"import sys; print(sys.version)\"` 看真实报错", d.Python.Path, d.Python.Error)
	}

	// 一件事都没查到：这是**失败**，要带 Hint（R3：不许空 Result 丢 Hint）。
	if !d.Python.OK && d.Node.Path == "" && d.Pnpm.Path == "" {
		hint := "这台机器上三样都没在 PATH 里；要么装一个，要么用 bash 手工确认它们的位置再告诉我"
		if d.Python.Path != "" {
			hint = fmt.Sprintf(`python 在 %s，但探针没跑通（%s）；先用 bash 手工跑 `+
				"`python -c \"import sys; print(sys.version)\"` 看真实报错", d.Python.Path, d.Python.Error)
		}
		return spi.Result{
			Summary: "没找到 python/node/pnpm",
			Hint:    hint,
		}, fmt.Errorf("wsdeps: python/node/pnpm 都不可用")
	}

	return res, nil
}

// run 跑一条子进程命令，返回 stdout；出错时把**原因**带回来。
//
// 为什么不能只返回 err：Windows 上被 context 掐断的子进程只会给 "exit status 1"，
// 与「python 真的退出 1」看不出区别（实测；不是 context.DeadlineExceeded）。
// 所以先用 c.Err() 判预算，再把 stderr 摘要附上——否则调用方只能拿到一个
// 无声的 ok:false，真正的原因（超时 / 缺 DLL / 语法错）全丢在管道里。
func run(ctx context.Context, budget time.Duration, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	cmd := exec.CommandContext(c, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cErr := c.Err(); cErr != nil {
			return stdout.String(), fmt.Errorf("%v（%s 预算用尽，子进程被掐断）", cErr, budget)
		}
		if s := oneLine(stderr.String(), 400); s != "" {
			return stdout.String(), fmt.Errorf("%w（stderr: %s）", err, s)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func orNA(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// oneLine 把子进程错误输出压成一行（保留可读性，按 rune 截断不劈开中文）。
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-wsdeps",
		Backend:  kernel.BackendStatic,
		Requires: []string{"tools"},
		Register: func(s *kernel.Scope) error {
			reg, err := kernel.Inject(s, spi.ToolsKey)
			if err != nil {
				return err
			}
			return reg.Register(&Tool{})
		},
	}
}
