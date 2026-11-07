package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/internal/transport"
)

type contextUsageSnapshot struct {
	max        int
	categories *[]transport.ContextCategory
	sections   *[]transport.ContextItemView
	files      *[]transport.ContextFileView
	skills     *[]transport.ContextItemView
	tools      *[]transport.ContextToolView
}

// parseContextUsageSnapshot 严格区分 absent 与显式 null。Go 直接把 null/缺字段
// 解进 int 会得到 0，若不做这层 RawMessage 校验，就会把“没有计量”伪造成真实 0。
func parseContextUsageSnapshot(raw json.RawMessage) (contextUsageSnapshot, error) {
	if isContextNull(raw) {
		return contextUsageSnapshot{}, fmt.Errorf("request/context 不能是 null")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return contextUsageSnapshot{}, fmt.Errorf("request/context 必须是非 null JSON 对象: %w", err)
	}
	if root == nil {
		return contextUsageSnapshot{}, fmt.Errorf("request/context 必须是非 null JSON 对象")
	}
	out := contextUsageSnapshot{}
	if rawWindow, exists := root["contextWindow"]; exists {
		value, err := requiredContextTokens(rawWindow, "request/context.contextWindow", true)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
		out.max = value
	}
	rawUsage, exists := root["contextUsage"]
	if !exists {
		return out, nil
	}
	if isContextNull(rawUsage) {
		return contextUsageSnapshot{}, fmt.Errorf("request/context.contextUsage 不能是 null")
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(rawUsage, &usage); err != nil {
		return contextUsageSnapshot{}, fmt.Errorf("request/context.contextUsage 必须是对象: %w", err)
	}
	if usage == nil {
		return contextUsageSnapshot{}, fmt.Errorf("request/context.contextUsage 必须是对象")
	}
	var err error
	if rawCategories, ok := usage["categories"]; ok {
		out.categories, err = decodeContextArray(rawCategories, "categories", parseContextCategory)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
	}
	if rawSections, ok := usage["sections"]; ok {
		out.sections, err = decodeContextArray(rawSections, "sections", parseContextItem)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
	}
	if rawFiles, ok := usage["files"]; ok {
		out.files, err = decodeContextArray(rawFiles, "files", parseContextFile)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
	}
	if rawSkills, ok := usage["skills"]; ok {
		out.skills, err = decodeContextArray(rawSkills, "skills", parseContextItem)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
	}
	if rawTools, ok := usage["tools"]; ok {
		out.tools, err = decodeContextArray(rawTools, "tools", parseContextTool)
		if err != nil {
			return contextUsageSnapshot{}, err
		}
	}
	return out, nil
}

func isContextNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeContextObject(raw json.RawMessage, kind string) (map[string]json.RawMessage, error) {
	if isContextNull(raw) {
		return nil, fmt.Errorf("context.usage %s 含 null 行", kind)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("context.usage %s 行必须是对象: %w", kind, err)
	}
	return object, nil
}

func decodeContextArray[T any](raw json.RawMessage, kind string, parse func(json.RawMessage) (T, error)) (*[]T, error) {
	if isContextNull(raw) {
		return nil, fmt.Errorf("context.usage.%s 不能是 null；缺省请省略字段", kind)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("context.usage.%s 必须是数组: %w", kind, err)
	}
	out := make([]T, 0, len(rows))
	for _, rawRow := range rows {
		row, err := parse(rawRow)
		if err != nil {
			return nil, fmt.Errorf("context.usage.%s: %w", kind, err)
		}
		out = append(out, row)
	}
	return &out, nil
}

func requiredContextString(object map[string]json.RawMessage, key, kind string) (string, error) {
	raw, exists := object[key]
	if !exists || isContextNull(raw) {
		return "", fmt.Errorf("%s 缺 %s 或显式为 null", kind, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("%s.%s 必须是非空字符串", kind, key)
	}
	return value, nil
}

func requiredContextTokens(raw json.RawMessage, kind string, positive bool) (int, error) {
	if isContextNull(raw) {
		return 0, fmt.Errorf("%s 缺 token 计量或显式为 null", kind)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%s 必须是整数: %w", kind, err)
	}
	return contextSafeInt(value, kind, positive)
}

func requiredContextRowTokens(object map[string]json.RawMessage, kind string) (int, error) {
	raw, exists := object["tokens"]
	if !exists {
		return 0, fmt.Errorf("%s 缺 tokens", kind)
	}
	return requiredContextTokens(raw, kind+".tokens", false)
}

func parseContextCategory(raw json.RawMessage) (transport.ContextCategory, error) {
	object, err := decodeContextObject(raw, "categories")
	if err != nil {
		return transport.ContextCategory{}, err
	}
	name, err := requiredContextString(object, "name", "category")
	if err != nil {
		return transport.ContextCategory{}, err
	}
	kind, err := requiredContextString(object, "kind", "category")
	if err != nil {
		return transport.ContextCategory{}, err
	}
	tokens, err := requiredContextRowTokens(object, "category")
	if err != nil {
		return transport.ContextCategory{}, err
	}
	return transport.ContextCategory{Name: name, Tokens: tokens, Kind: kind}, nil
}

func parseContextItem(raw json.RawMessage) (transport.ContextItemView, error) {
	object, err := decodeContextObject(raw, "items")
	if err != nil {
		return transport.ContextItemView{}, err
	}
	name, err := requiredContextString(object, "name", "item")
	if err != nil {
		return transport.ContextItemView{}, err
	}
	tokens, err := requiredContextRowTokens(object, "item")
	if err != nil {
		return transport.ContextItemView{}, err
	}
	return transport.ContextItemView{Name: name, Tokens: tokens}, nil
}

func parseContextFile(raw json.RawMessage) (transport.ContextFileView, error) {
	object, err := decodeContextObject(raw, "files")
	if err != nil {
		return transport.ContextFileView{}, err
	}
	path, err := requiredContextString(object, "path", "file")
	if err != nil {
		return transport.ContextFileView{}, err
	}
	tokens, err := requiredContextRowTokens(object, "file")
	if err != nil {
		return transport.ContextFileView{}, err
	}
	return transport.ContextFileView{Path: path, Tokens: tokens}, nil
}

func parseContextTool(raw json.RawMessage) (transport.ContextToolView, error) {
	object, err := decodeContextObject(raw, "tools")
	if err != nil {
		return transport.ContextToolView{}, err
	}
	name, err := requiredContextString(object, "name", "tool")
	if err != nil {
		return transport.ContextToolView{}, err
	}
	tokens, err := requiredContextRowTokens(object, "tool")
	if err != nil {
		return transport.ContextToolView{}, err
	}
	tool := transport.ContextToolView{Name: name, Tokens: tokens}
	if rawServer, exists := object["server"]; exists {
		if isContextNull(rawServer) {
			return transport.ContextToolView{}, fmt.Errorf("tool.server 不能是 null")
		}
		if err := json.Unmarshal(rawServer, &tool.Server); err != nil || tool.Server == "" {
			return transport.ContextToolView{}, fmt.Errorf("tool.server 存在时必须是非空字符串")
		}
	}
	return tool, nil
}
