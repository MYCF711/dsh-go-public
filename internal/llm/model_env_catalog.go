package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// envProviderModel 是 OpenAI /v1/models 之外、由当前 provider 实际返回的
// 模型能力元数据。DSH_GO_MODEL 本身只选择 id，不提供这些真值。
type envProviderModel struct {
	ID                       string   `json:"id"`
	DisplayName              string   `json:"display_name"`
	MagpieLabel              string   `json:"magpie_label"`
	OwnedBy                  string   `json:"owned_by"`
	ContextWindow            int64    `json:"context_window"`
	ContextLength            int64    `json:"context_length"`
	MaxOutputTokens          int64    `json:"max_output_tokens"`
	Reasoning                *bool    `json:"reasoning"`
	ReasoningLevels          []string `json:"reasoning_levels"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	Modalities struct {
		Input []string `json:"input"`
	} `json:"modalities"`
}

// DiscoverEnvModelCatalogEntry 从当前进程配置的 provider 读取显式 env model
// 的真实 catalog entry。只有 id 精确匹配才返回 found；缺少的能力保持缺省，
// 尤其不从 reasoning=true、模型名或宿主默认值编造 reasoningEfforts。
func DiscoverEnvModelCatalogEntry(ctx context.Context, client *Client, modelID, apiKeyEnv string) (json.RawMessage, bool, error) {
	if client == nil {
		return nil, false, fmt.Errorf("env 模型发现缺 LLM client")
	}
	if err := validateCatalogID(modelID); err != nil {
		return nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.BaseURL+"models", nil)
	if err != nil {
		return nil, false, err
	}
	if client.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+client.APIKey)
	}
	if client.UserAgent != "" {
		req.Header.Set("User-Agent", client.UserAgent)
	}
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("发现 env 模型元数据失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		message := RedactCredential(strings.TrimSpace(string(body)), client.APIKey)
		return nil, false, fmt.Errorf("env 模型元数据端点回 %d：%s", resp.StatusCode, message)
	}
	var document struct {
		Data *[]envProviderModel `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&document); err != nil {
		return nil, false, fmt.Errorf("env 模型元数据不是合法 JSON: %w", err)
	}
	if document.Data == nil {
		return nil, false, fmt.Errorf("env 模型元数据缺 data 数组")
	}
	var match *envProviderModel
	for i := range *document.Data {
		candidate := &(*document.Data)[i]
		if candidate.ID != modelID {
			continue
		}
		if match != nil {
			return nil, false, fmt.Errorf("env 模型元数据含重复 id %q", modelID)
		}
		match = candidate
	}
	if match == nil {
		return nil, false, nil
	}
	raw, err := envCatalogEntryJSON(*match, strings.TrimSuffix(client.BaseURL, "/"), apiKeyEnv)
	if err != nil {
		return nil, false, err
	}
	if _, err := validateCatalogModel(raw); err != nil {
		return nil, false, fmt.Errorf("env 模型 %q 的 provider 元数据不能投影为 catalog: %w", modelID, err)
	}
	return raw, true, nil
}

func envCatalogEntryJSON(model envProviderModel, baseURL, apiKeyEnv string) (json.RawMessage, error) {
	contextWindow := model.ContextWindow
	if contextWindow == 0 {
		contextWindow = model.ContextLength
	}
	name := model.DisplayName
	if name == "" {
		name = model.MagpieLabel
	}
	var reasoning json.RawMessage
	levels := append([]string(nil), model.ReasoningLevels...)
	if len(model.SupportedReasoningLevels) > 0 {
		levels = levels[:0]
		for _, level := range model.SupportedReasoningLevels {
			levels = append(levels, level.Effort)
		}
	}
	if len(levels) > 0 {
		var buffer bytes.Buffer
		buffer.WriteByte('{')
		seen := map[string]bool{}
		for i, level := range levels {
			if level == "" || seen[level] {
				return nil, fmt.Errorf("env 模型 %q 含空或重复 reasoning level %q", model.ID, level)
			}
			seen[level] = true
			if i > 0 {
				buffer.WriteByte(',')
			}
			key, _ := json.Marshal(level)
			wire, _ := json.Marshal(level)
			buffer.Write(key)
			buffer.WriteByte(':')
			buffer.Write(wire)
		}
		buffer.WriteByte('}')
		reasoning = json.RawMessage(buffer.Bytes())
	} else if model.Reasoning != nil && !*model.Reasoning {
		reasoning = json.RawMessage(`false`)
	}
	entry := struct {
		ID               string          `json:"id"`
		Name             string          `json:"name,omitempty"`
		OwnedBy          string          `json:"owned_by,omitempty"`
		API              string          `json:"api"`
		BaseURL          string          `json:"baseURL"`
		APIKeyEnv        string          `json:"apiKeyEnv,omitempty"`
		ContextWindow    int64           `json:"contextWindow,omitempty"`
		MaxTokens        int64           `json:"maxTokens,omitempty"`
		Input            []string        `json:"input,omitempty"`
		ReasoningEfforts json.RawMessage `json:"reasoningEfforts,omitempty"`
	}{
		ID: model.ID, Name: name, OwnedBy: model.OwnedBy,
		API: "openai-completions", BaseURL: baseURL, APIKeyEnv: apiKeyEnv,
		ContextWindow: contextWindow, MaxTokens: model.MaxOutputTokens,
		Input: append([]string(nil), model.Modalities.Input...), ReasoningEfforts: reasoning,
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("序列化 env 模型 %q 元数据: %w", model.ID, err)
	}
	return json.RawMessage(raw), nil
}
