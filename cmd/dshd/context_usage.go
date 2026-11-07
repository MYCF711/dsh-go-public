package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"dshgo/codecs/v4"
	"dshgo/internal/transport"
)

const maxContextSafeInteger = int64(1<<53 - 1)

// ContextUsage 只投影最近一次 assistant 同时代的真实记录。读路径不重拼提示、
// 不写 v4，也不用仅供压缩的启发式 Meter 伪造 full token-count。
func (s *sessionSource) ContextUsage(sessionID, detail string) (transport.ContextUsageView, error) {
	if detail != "full" {
		return transport.ContextUsageView{}, fmt.Errorf("context.usage detail 只支持 full")
	}
	path, err := s.resolve(sessionID)
	if err != nil {
		return transport.ContextUsageView{}, err
	}
	lg, err := v4.ReadFile(path)
	if err != nil {
		return transport.ContextUsageView{}, err
	}
	return s.contextUsageFromV4(lg)
}

type v4LastUsage struct {
	InputTokens  *int64 `json:"inputTokens"`
	OutputTokens *int64 `json:"outputTokens"`
	TotalTokens  *int64 `json:"totalTokens"`
}

func nonNegativeMeasured(value *int64, field string) (int, error) {
	if value == nil {
		return 0, fmt.Errorf("最近 assistant usage 缺 %s 真值", field)
	}
	return contextSafeInt(*value, field, false)
}

func contextSafeInt(value int64, field string, positive bool) (int, error) {
	if value < 0 || (positive && value == 0) || value > maxContextSafeInteger || int64(int(value)) != value {
		return 0, fmt.Errorf("%s 必须是%s安全整数，实际 %d", field, map[bool]string{true: "正", false: "非负"}[positive], value)
	}
	return int(value), nil
}

func (s *sessionSource) contextUsageFromV4(lg *v4.Log) (transport.ContextUsageView, error) {
	lastAssistant := -1
	var assistantRaw json.RawMessage
	for i := len(lg.Events) - 1; i >= 0; i-- {
		if lg.Events[i].Type == "assistant/message" {
			lastAssistant = i
			assistantRaw = lg.Events[i].Data
			break
		}
	}
	if lastAssistant < 0 {
		return transport.ContextUsageView{}, errors.New("context.usage 没有 assistant usage，不能补零")
	}
	var meta struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(assistantRaw, &meta); err != nil {
		return transport.ContextUsageView{}, fmt.Errorf("最近 assistant/message 不是合法 JSON: %w", err)
	}
	if len(meta.Usage) == 0 || bytes.Equal(bytes.TrimSpace(meta.Usage), []byte("null")) {
		return transport.ContextUsageView{}, errors.New("context.usage 最近 assistant 没有 usage，不能补零或回退旧值")
	}
	var last v4LastUsage
	if err := json.Unmarshal(meta.Usage, &last); err != nil {
		return transport.ContextUsageView{}, fmt.Errorf("最近 assistant usage 非法: %w", err)
	}
	used, err := nonNegativeMeasured(last.TotalTokens, "totalTokens")
	if err != nil {
		return transport.ContextUsageView{}, err
	}
	// input/output 若存在必须合法，但它们是 API 计费方向，不是 full 的
	// context contributors，绝不拿它们拼 system/tools/messages 假分类。
	for _, field := range []struct {
		name  string
		value *int64
	}{{"inputTokens", last.InputTokens}, {"outputTokens", last.OutputTokens}} {
		if field.value != nil {
			if _, err := nonNegativeMeasured(field.value, field.name); err != nil {
				return transport.ContextUsageView{}, err
			}
		}
	}
	// usage 之后又出现会改变模型可见 surface 的事件时，旧 totalTokens 已不是
	// 查询时的 current context。没有新计量就 fail-loud，不能返回陈旧精确值。
	for _, ev := range lg.Events[lastAssistant+1:] {
		switch ev.Type {
		case "system/message", "developer/message", "user/message", "tool/result", "compaction/summary", "compaction/prune":
			return transport.ContextUsageView{}, fmt.Errorf("context.usage 最近 assistant usage 后 surface 已被 %s 改变，需要新计量", ev.Type)
		}
	}

	result := transport.ContextUsageView{
		Used: used,
		// 没有真实 breakdown 时只给一个聚合类；这与 summary 一样诚实，
		// 但 full 不会把 summary 伪造成 sections/files/skills/tools。
		Categories: []transport.ContextCategory{{Name: "context", Tokens: used, Kind: "used"}},
	}
	var requestContext json.RawMessage
	for _, ev := range lg.Events[:lastAssistant] {
		if ev.Type == "request/context" {
			requestContext = append(requestContext[:0], ev.Data...)
		}
	}
	if len(requestContext) != 0 {
		snapshot, err := parseContextUsageSnapshot(requestContext)
		if err != nil {
			return transport.ContextUsageView{}, err
		}
		result.Max = snapshot.max
		if snapshot.categories != nil {
			result.Categories = *snapshot.categories
		}
		result.Sections = snapshot.sections
		result.Files = snapshot.files
		result.Skills = snapshot.skills
		result.Tools = snapshot.tools
	}
	if err := validateContextDetails(&result); err != nil {
		return transport.ContextUsageView{}, err
	}
	return result, nil
}

func validateContextDetails(view *transport.ContextUsageView) error {
	if _, err := contextSafeInt(int64(view.Used), "context.usage.used", false); err != nil {
		return err
	}
	if view.Max != 0 {
		if _, err := contextSafeInt(int64(view.Max), "context.usage.max", true); err != nil {
			return err
		}
	}
	if view.Categories == nil {
		return fmt.Errorf("context.usage.categories 必须是数组，不能是 null")
	}
	valid := func(name string, tokens int) error {
		if name == "" {
			return fmt.Errorf("context.usage details 含空名称")
		}
		_, err := contextSafeInt(int64(tokens), "context.usage detail.tokens", false)
		return err
	}
	for _, row := range view.Categories {
		if err := valid(row.Name, row.Tokens); err != nil || row.Kind == "" {
			return fmt.Errorf("context.usage categories 含空名称/kind 或非法 token 数")
		}
	}
	if view.Sections != nil {
		for _, row := range *view.Sections {
			if err := valid(row.Name, row.Tokens); err != nil {
				return err
			}
		}
	}
	if view.Skills != nil {
		for _, row := range *view.Skills {
			if err := valid(row.Name, row.Tokens); err != nil {
				return err
			}
		}
	}
	if view.Files != nil {
		for _, row := range *view.Files {
			if err := valid(row.Path, row.Tokens); err != nil {
				return err
			}
		}
	}
	if view.Tools != nil {
		for _, row := range *view.Tools {
			if err := valid(row.Name, row.Tokens); err != nil {
				return err
			}
		}
	}
	return nil
}
