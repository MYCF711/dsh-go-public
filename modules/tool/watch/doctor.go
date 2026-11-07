// watch_doctor：登记完立刻能问一句"这套配置真的能用吗"。
//
// 为什么需要：`watch_start` 只把名单落盘，**不验证任何东西**。gh 没登录、仓库没权限、
// ack 标签根本不存在——这些都会拖到第一次 `dshd watch check` 才炸，而那时候
// 用户已经在等"有没有新 issue"了，报错看起来像"宿主坏了"。
//
// 这个工具把那层检查提前：它只读、只问，不改任何状态。
package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"dshgo/internal/spi"
)

// runCmd 起一个外部命令并返回合并输出。
//
// 做成包级变量**只为可测**：测试里替换成假实现，就不必真的装 gh、真的联网。
// 生产路径上它就是 exec.CommandContext。
var runCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// doctorRepo 是单个仓库的检查结果。
type doctorRepo struct {
	Repo     string   `json:"repo"`
	OK       bool     `json:"ok"`
	Checked  []string `json:"checked"`  // 通过了的检查项（让人知道"验过什么"）
	Problems []string `json:"problems"` // 没通过的（这就是要拿去修的东西）
}

// doctorResult 是整个工具的产出。
type doctorResult struct {
	Gh struct {
		OK      bool   `json:"ok"`
		Version string `json:"version,omitempty"`
		Problem string `json:"problem,omitempty"`
	} `json:"gh"`
	Workdir   string       `json:"workdir"`
	WorkdirOK bool         `json:"workdirOK"`
	Repos     []doctorRepo `json:"repos"`
	Ready     bool         `json:"ready"` // 全部通过才为真
}

// doctor 逐仓库体检。
//
// 关键约定：**任何一项检查失败都不让工具失败**。失败信息进 problems，
// 工具本身仍返回 nil error——因为"gh 没登录"是它要报告的事实，不是它执行不下去。
// 只有连名单都读不出来才返回 err（那是接线问题，不是配置问题）。
func (t *Tool) doctor(ctx context.Context) (spi.Result, error) {
	if err := t.st.ready(); err != nil {
		return spi.Result{Hint: "状态目录没配上：这是接线问题，报告监工，别改配置自己试"}, err
	}

	t.st.mu.Lock()
	list, err := t.st.load()
	t.st.mu.Unlock()
	if err != nil {
		return spi.Result{
			Hint: fmt.Sprintf("名单读不出来：看一眼 %s 是不是被手改坏了，改回合法 JSON 或删掉它重新登记", t.st.display()),
		}, err
	}

	if len(list) == 0 {
		return spi.Result{
			Summary: "还没登记任何仓库",
			Text:    "还没登记任何仓库——没有可体检的对象。",
			Data:    map[string]any{"action": "doctor", "total": 0, "repos": []doctorRepo{}},
			Hint:    `先 watch_start 登记一个仓库，例如 {"repo":"owner/name"}，再来体检`,
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var out doctorResult
	out.Workdir = t.st.display()
	if st, statErr := os.Stat(t.st.dir); statErr == nil && st.IsDir() {
		out.WorkdirOK = true
	}

	// gh 本体与登录态：这两项不过，后面逐仓库的检查没有意义，但仍然继续跑——
	// 让用户一次看到全部问题，比修一个再跑一次强。
	ghOK := false
	if buf, err := runCmd(ctx, "gh", "--version"); err == nil {
		ghOK = true
		out.Gh.OK = true
		out.Gh.Version = firstLine(string(buf))
	} else {
		out.Gh.Problem = "gh 不可用：" + firstLine(string(buf)) + "（装 GitHub CLI 并放进 PATH）"
	}
	if ghOK {
		if buf, err := runCmd(ctx, "gh", "auth", "status"); err != nil {
			out.Gh.OK = false
			out.Gh.Problem = "gh 未登录：" + firstLine(string(buf)) + "（跑 gh auth login）"
		}
	}

	allOK := out.Gh.OK && out.WorkdirOK
	for _, w := range list {
		r := doctorRepo{Repo: w.Repo}
		var problems []string

		if !out.WorkdirOK {
			problems = append(problems, "状态目录不存在："+out.Workdir)
		}
		if !out.Gh.OK {
			problems = append(problems, "gh 不可用或未登录")
		}

		if out.Gh.OK {
			// 仓库能不能访问。用 api 而不是 repo view：错误信息更直白。
			if buf, err := runCmd(ctx, "gh", "api", "repos/"+w.Repo, "-q", ".full_name"); err != nil {
				problems = append(problems, "仓库访问不了 "+w.Repo+"："+firstLine(string(buf)))
			} else {
				r.Checked = append(r.Checked, "仓库可访问")
			}

			// ack 标签存不存在——不存在的话，处理完打标那一刻才会炸。
			label := w.AckLabel
			if strings.TrimSpace(label) == "" {
				label = defaultAckLabel
			}
			if buf, err := runCmd(ctx, "gh", "label", "list", "-R", w.Repo, "--limit", "200"); err == nil {
				if !strings.Contains(string(buf), label) {
					problems = append(problems, "打标用的标签不存在："+label+"（先在仓库里建它，或 watch_start 换一个）")
				} else {
					r.Checked = append(r.Checked, "标签存在："+label)
				}
			} else {
				problems = append(problems, "列标签失败："+firstLine(string(buf)))
			}
		}

		r.Problems = problems
		r.OK = len(problems) == 0
		if !r.OK {
			allOK = false
		}
		out.Repos = append(out.Repos, r)
	}
	out.Ready = allOK

	data, _ := json.MarshalIndent(out, "", "  ")
	bad := 0
	for _, r := range out.Repos {
		if !r.OK {
			bad++
		}
	}

	if !out.Ready {
		// 有不通过的：Summary 说清"几个没过"，正文给可读清单，Hint 给下一步。
		var b strings.Builder
		if out.Gh.Problem != "" {
			fmt.Fprintf(&b, "gh：%s\n", out.Gh.Problem)
		}
		for _, r := range out.Repos {
			for _, p := range r.Problems {
				fmt.Fprintf(&b, "%s：%s\n", r.Repo, p)
			}
		}
		return spi.Result{
			Summary: fmt.Sprintf("体检没过：%d 个仓库待修", bad),
			Text:    strings.TrimRight(b.String(), "\n"),
			Data:    out,
			Hint:    "按上面每条修：gh auth login / 确认仓库权限 / 在仓库里建好打标标签，然后重跑 watch_doctor",
		}, nil
	}

	return spi.Result{
		Summary: fmt.Sprintf("体检通过：%d 个仓库都能查、能打标", len(out.Repos)),
		Text:    string(data),
		Data:    out,
	}, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
