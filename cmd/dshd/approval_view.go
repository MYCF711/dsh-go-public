package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// describeApproval 决定"审批时用户看到什么"。
//
// 这里有一条不能让步的规矩：**给用户看的是要执行什么，不是模型说它要干什么。**
//
// 真实事故形态：模型把 `rm -rf /tmp/x` 的 description 写成"看看临时目录"。
// 如果审批框只显示 description，用户点 y 是在批准一句描述，不是在批准一条命令。
// 所以：
//   - 命令永远原样、优先展示；
//   - 模型的 description 单独一行，并明确标成"模型自述"（不可当依据）；
//   - 命令太长时截中间而不是截尾巴——`rm -rf ...` 的危险部分常在尾部。
func describeApproval(tool, args string) string {
	switch tool {
	case "bash":
		var a struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal([]byte(args), &a); err == nil && a.Command != "" {
			out := "命令: " + clipMiddle(a.Command, 300)
			if d := strings.TrimSpace(a.Description); d != "" {
				out += string(rune(10)) + "模型自述（仅供参考，不是依据）: " + clipMiddle(d, 120)
			}
			return out
		}
	case "write":
		var a struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal([]byte(args), &a); err == nil && a.FilePath != "" {
			return fmt.Sprintf("写入 %s（%d 字节，整体覆盖）", a.FilePath, len(a.Content))
		}
	case "edit":
		var a struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal([]byte(args), &a); err == nil && a.FilePath != "" {
			return fmt.Sprintf("修改 %s（替换 %d 字节 → %d 字节）",
				a.FilePath, len(a.OldString), len(a.NewString))
		}
	}
	return clipMiddle(args, 300)
}

// clipMiddle 太长的串截中间：危险的部分常在结尾（`rm -rf /`、`> /dev/sda`）。
func clipMiddle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := n * 2 / 3
	tail := n - head
	return string(r[:head]) + " …(省略 " + fmt.Sprint(len(r)-n) + " 字符)… " + string(r[len(r)-tail:])
}
