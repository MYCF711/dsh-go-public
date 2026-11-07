// Package ask 提供 `ask_user_question` 工具：向人提一个或多个问题，并**结束本轮**。
//
// 它是唯一一个"结果不是结果"的工具：它返回的是等回答的问题。
// 所以它设 `Result.Stop`——agent 循环在这一步之后收尾，把控制权交回人。
// 不这么做的话，模型会自己编一个答案继续往下跑，整条链路就变成了自问自答。
//
// 契约照抄上游（research/20-dsh-tool-schemas.json）：外层是 **questions 数组**，
// 每项带 `id`（回答里回显）与 `question`，可选 `header` / `options` / `multi_select`。
package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/internal/kernel"
	"dshgo/internal/spi"
)

var schema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "questions": {
      "type": "array",
      "description": "要先问人的问题；一次问全，别挤成多轮",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "这个问题的稳定 id；回答里会回显"},
          "question": {"type": "string", "description": "具体要问的那句话"},
          "header": {"type": "string", "description": "可选的短标题，例如「确认」「选模式」"},
          "options": {
            "type": "array",
            "description": "可选候选；推荐项放第一个并在标签后加「（推荐）」",
            "items": {
              "type": "object",
              "properties": {
                "label": {"type": "string", "description": "给人看的一句话选项"},
                "description": {"type": "string", "description": "这个选择的取舍，可选"}
              },
              "required": ["label"],
              "additionalProperties": true
            }
          },
          "multi_select": {"type": "boolean", "description": "能不能多选，默认不能"}
        },
        "required": ["id", "question"],
        "additionalProperties": true
      }
    }
  },
  "required": ["questions"],
  "additionalProperties": false
}`)

const (
	maxQuestions = 4
	maxOptions   = 6
)

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Question struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

type args struct {
	Questions []struct {
		ID          string   `json:"id"`
		Question    string   `json:"question"`
		Header      string   `json:"header"`
		Options     []Option `json:"options"`
		MultiSelect bool     `json:"multi_select"`
	} `json:"questions"`
}

type data struct {
	Questions      []Question `json:"questions"`
	Count          int        `json:"count"`
	AwaitingAnswer bool       `json:"awaiting_answer"`
}

type Tool struct{}

func (t *Tool) Name() string            { return "ask_user_question" }
func (t *Tool) Caps() spi.Caps          { return spi.Caps{} }
func (t *Tool) Schema() json.RawMessage { return schema }
func (t *Tool) Description() string {
	return "向人提问（可一次问多个）并等回答，本轮到此结束。需要人拍板时用，别自己猜。"
}

func (t *Tool) Exec(ctx context.Context, raw json.RawMessage) (spi.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return spi.Result{Hint: `参数不合法：期望 {"questions": [{"id": "q1", "question": "…", "options"?: [{"label": "…"}]}]}`}, err
	}
	if a.Questions == nil {
		return spi.Result{Hint: `必须给 questions（数组）：只是想说明就别调这个工具`},
			fmt.Errorf("缺 questions")
	}
	if len(a.Questions) == 0 {
		return spi.Result{Hint: "questions 是空的：没有问题就别调这个工具"},
			fmt.Errorf("questions 为空")
	}
	if len(a.Questions) > maxQuestions {
		return spi.Result{Hint: fmt.Sprintf("一次最多 %d 个问题：问全，但别拿清单淹人", maxQuestions)},
			fmt.Errorf("问题过多: %d", len(a.Questions))
	}

	seen := map[string]bool{}
	out := make([]Question, 0, len(a.Questions))
	for i, q := range a.Questions {
		id := strings.TrimSpace(q.ID)
		if id == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 个问题没有 id；给一个短 id（如 q1），回答里靠它对应", i+1)},
				fmt.Errorf("第 %d 个问题缺 id", i+1)
		}
		if seen[id] {
			return spi.Result{Hint: fmt.Sprintf("id %q 重复了；每个问题要有唯一 id，不然回答对不上号", id)},
				fmt.Errorf("id 重复: %s", id)
		}
		seen[id] = true

		text := strings.TrimSpace(q.Question)
		if text == "" {
			return spi.Result{Hint: fmt.Sprintf("第 %d 个问题（%s）没有 question 正文", i+1, id)},
				fmt.Errorf("第 %d 个问题为空", i+1)
		}
		if len(q.Options) > maxOptions {
			return spi.Result{Hint: fmt.Sprintf("「%s」有 %d 个选项：最多 %d 个，人挑不动", id, len(q.Options), maxOptions)},
				fmt.Errorf("%s 选项过多", id)
		}
		if len(q.Options) == 1 {
			return spi.Result{Hint: fmt.Sprintf("「%s」只有一个选项，不算选择：补上第二个，或者干脆不给 options 让人自己写", id)},
				fmt.Errorf("%s 只有一个选项", id)
		}

		opts := make([]Option, 0, len(q.Options))
		for j, o := range q.Options {
			label := strings.TrimSpace(o.Label)
			if label == "" {
				return spi.Result{Hint: fmt.Sprintf("「%s」第 %d 个选项没有 label", id, j+1)},
					fmt.Errorf("选项为空")
			}
			opts = append(opts, Option{Label: label, Description: strings.TrimSpace(o.Description)})
		}

		out = append(out, Question{
			ID: id, Question: text,
			Header: strings.TrimSpace(q.Header), Options: opts,
			MultiSelect: q.MultiSelect,
		})
	}

	// 模型看到的正文：问题 + 选项。它不该在这之后继续干活。
	var b strings.Builder
	for i, q := range out {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if q.Header != "" {
			fmt.Fprintf(&b, "【%s】", q.Header)
		}
		fmt.Fprintf(&b, "%s（id: %s）", q.Question, q.ID)
		for j, o := range q.Options {
			fmt.Fprintf(&b, "\n  %d) %s", j+1, o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, "——%s", o.Description)
			}
		}
		if q.MultiSelect {
			b.WriteString("\n  （可多选）")
		}
	}

	summary := "等你回答：" + oneLine(out[0].Question)
	if len(out) > 1 {
		summary = fmt.Sprintf("等你回答 %d 个问题：%s", len(out), oneLine(out[0].Question))
	}

	return spi.Result{
		Summary: summary,
		Text:    b.String(),
		Data:    data{Questions: out, Count: len(out), AwaitingAnswer: true},
		Hint:    "本轮到此为止：把人答的内容当作下一条用户消息继续，不要自己编答案",
		Stop:    true,
	}, nil
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 60 {
		return string([]rune(s)[:60]) + "…"
	}
	return s
}

// Module 返回装配用的模块描述。
func Module() kernel.Module {
	return kernel.Module{
		ID:       "tool-ask",
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
