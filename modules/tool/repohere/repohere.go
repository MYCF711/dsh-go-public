// Package repohere 提供 `repo_here` 工具：报告当前目录属于哪个 GitHub 仓库。
//
// 为什么需要它：模型经常要"给这个仓库开个 issue / 看看 PR"，
// 于是先 `bash git remote -v` 再自己拼 owner/name——两三条命令换一个字符串。
// 这里一次给全：owner、name、full_name、当前分支、原始 remote。
//
// 边界感：
//   - 只认 github.com（https 与 scp 两种写法）：别的托管平台不猜，原样回给模型
//   - 只读 git 配置，不起副作用；分支取不到就留空，不算失败
//   - 失败按原因分三种，各给一条能直接照做的 Hint（没仓库 / 没 origin / 不是 GitHub）
//   - remote 里可能带凭据（https://user:token@github.com/...），回显前要打码
package repohere

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

const (
	// 每种失败一句，措辞对齐"下一步做什么"，不是"哪里错了"。
	hintNotRepo  = "当前目录不在 git 仓库里：用 bash `ls -a` 看看有没有 .git，或 working_directory 确认自己在哪；要查别的目录就 bash `git -C <dir> remote get-url origin`"
	hintNoOrigin = `这个仓库没有 origin 远端：先 git remote add origin git@github.com:owner/name.git（或 https 地址），配好再跑一次`
	hintNoGit    = "找不到 git 命令：确认 PATH 里有 git；不想装的话用 read 看 .git/config"
)

type Tool struct{}

func (t *Tool) Name() string { return "repo_here" }
func (t *Tool) Caps() spi.Caps {
	// 只读 git 配置（git remote get-url / branch --show-current 都不改任何东西）。
	return spi.Caps{}
}
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "报告当前目录属于哪个 GitHub 仓库（owner/name 与分支）。"
}

// data 是成功时的结构化载荷。键名 snake_case 由序列化层统一处理，这里显式写上。
type data struct {
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Branch   string `json:"branch"`
	Remote   string `json:"remote"`
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	// 无参数工具仍然校验一次：Schema 是 additionalProperties:false，
	// 模型臆造参数时要当场说清，别让它以为参数生效了。
	if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return spi.Result{Hint: `repo_here 不接受参数：直接调用 {"type":"object","properties":{}} 即可`},
				fmt.Errorf("参数不合法: %w", err)
		}
		if len(probe) > 0 {
			keys := make([]string, 0, len(probe))
			for k := range probe {
				keys = append(keys, k)
			}
			return spi.Result{Hint: "repo_here 不接受参数：它只报告当前目录的仓库信息，直接调用即可（多余的键：" + strings.Join(keys, ", ") + "）"},
				fmt.Errorf("多余参数: %s", strings.Join(keys, ", "))
		}
	}

	remote, stderr, err := gitOut(ctx, "remote", "get-url", "origin")
	if err != nil {
		return failure(ctx, remote, stderr, err)
	}
	if remote == "" {
		// 退出码 0 但没有 URL：少见（配置被写坏），仍按"没有可用的 origin"处理。
		return spi.Result{Hint: hintNoOrigin}, fmt.Errorf("origin 为空")
	}

	owner, name, ok := parseGitHub(remote)
	if !ok {
		hint := fmt.Sprintf("origin 是 %s，不是 github.com 仓库——本工具只认 github.com；要确认远端细节，用 bash 跑 `git remote -v`，或者直接读 .git/config", quoteShort(remote))
		return spi.Result{
			Text: "origin: " + maskCredentials(remote),
			Hint: hint,
		}, fmt.Errorf("不是 GitHub 仓库: %s", maskCredentials(remote))
	}

	// 分支取不到不影响结论：detached HEAD、空仓库都会失败，留空即可。
	branch, _, _ := gitOut(ctx, "branch", "--show-current")

	d := data{
		Owner:    owner,
		Name:     name,
		FullName: owner + "/" + name,
		Branch:   branch,
		Remote:   maskCredentials(remote),
	}

	summary := d.FullName + " @ " + branch
	if branch == "" {
		summary = d.FullName + " @ (detached)"
	}

	return spi.Result{
		Summary: summary,
		Data:    d,
	}, nil
}

// failure 把 `git remote get-url origin` 的失败按原因分类。
// 不靠退出码猜（都是 128），而是看 stderr 原文——这几句话是 git 的稳定输出。
func failure(ctx context.Context, stdout, stderr string, err error) (spi.Result, error) {
	if ctx.Err() != nil {
		return spi.Result{Hint: "调用被取消了：需要结果就再跑一次；不确定状态时先 bash `ls -a` 看看"},
			fmt.Errorf("已取消: %w", ctx.Err())
	}
	// 命令根本没起来（git 未安装）。
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return spi.Result{Hint: hintNoGit}, fmt.Errorf("找不到 git: %w", err)
	}

	msg := strings.ToLower(stderr + "\n" + stdout)
	switch {
	case strings.Contains(msg, "not a git repository"):
		return spi.Result{Hint: hintNotRepo}, fmt.Errorf("不是 git 仓库")
	case strings.Contains(msg, "no such remote"), strings.Contains(msg, "does not appear to be a git repository"):
		return spi.Result{Hint: hintNoOrigin}, fmt.Errorf("没有 origin 远端")
	}

	// 认不出来的失败：把 git 原话带回去，别自己编原因。
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		detail = strings.TrimSpace(stdout)
	}
	if detail == "" {
		detail = err.Error()
	}
	return spi.Result{
			Hint: "git 读配置失败（不是常见的三种原因）：先 bash `git remote -v` 看看，或直接 read .git/config",
		},
		fmt.Errorf("git 执行失败: %s", detail)
}

// gitOut 跑一条 git 子命令，返回（trim 过的 stdout, 原始 stderr, err）。
// 不用 CombinedOutput：stdout 是数据，stderr 是诊断，混在一起就没法分类了。
func gitOut(ctx context.Context, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return strings.TrimSpace(out.String()), errb.String(), err
}

// parseGitHub 从 remote URL 里取 owner/name，只认 github.com。
// 支持三种写法（以及带凭据的 https 变体）：
//
//	https://github.com/owner/name.git
//	git@github.com:owner/name.git
//	https://github.com/owner/name
func parseGitHub(remote string) (owner, name string, ok bool) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return "", "", false
	}

	// URL 形态：<scheme>://[user[:pass]@]github.com/owner/name[.git]
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return "", "", false
		}
		host := rest[:slash]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:] // 去掉凭据，只比主机名
		}
		host = strings.TrimSuffix(host, ":443")
		if !strings.EqualFold(host, "github.com") {
			return "", "", false
		}
		return splitOwnerName(rest[slash+1:])
	}

	// scp 形态：user@github.com:owner/name[.git]（前面不允许出现路径分隔符）
	if i := strings.Index(s, "github.com:"); i >= 0 {
		prefix := s[:i]
		if !strings.HasSuffix(prefix, "@") || strings.Contains(prefix, "/") {
			return "", "", false
		}
		return splitOwnerName(s[i+len("github.com:"):])
	}

	return "", "", false
}

// splitOwnerName 把 "owner/name.git" 拆成两段。多一层或少一层都算看不懂。
func splitOwnerName(p string) (string, string, bool) {
	p = strings.TrimSpace(p)
	p = strings.TrimRight(p, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "", "", false
	}
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// maskCredentials 把 URL 里的口令打码：remote 会进模型上下文与会话日志，
// 而 https 形式常常长成 https://x-access-token:<token>@github.com/...。
func maskCredentials(remote string) string {
	i := strings.Index(remote, "://")
	if i < 0 {
		return remote
	}
	rest := remote[i+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return remote
	}
	// 主机之前才算 userinfo：路径里出现 @ 不动它。
	if slash := strings.Index(rest, "/"); slash >= 0 && slash < at {
		return remote
	}
	userinfo := rest[:at]
	if c := strings.Index(userinfo, ":"); c >= 0 {
		userinfo = userinfo[:c] + ":***"
	}
	return remote[:i+3] + userinfo + rest[at:]
}

// quoteShort 给 Hint 里引用的 URL 加引号并压短：提示要能读完。
func quoteShort(s string) string {
	const max = 120
	rs := []rune(maskCredentials(s))
	if len(rs) > max {
		return `"` + string(rs[:max]) + `…"`
	}
	return `"` + string(rs) + `"`
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-repohere",
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
