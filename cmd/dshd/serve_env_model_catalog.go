package main

import (
	"context"
	"strings"

	"dshgo/internal/llm"
)

// configureEnvModelCatalog 只给物理缺失的 catalog 安装当前 env model 的
// provider 真值投影。DSH_GO_MODEL 经 W162 转成 --model；它自身不含能力元数据。
func configureEnvModelCatalog(ctx context.Context, catalog *llm.ModelCatalogStore, modelID, base, credential string) (bool, error) {
	if catalog == nil || !catalog.Missing() || strings.TrimSpace(modelID) == "" {
		return false, nil
	}
	apiKeyEnv := ""
	if strings.TrimSpace(credential) != "" {
		apiKeyEnv = "DSHD_LLM_KEY"
	}
	client := llm.New(base, credential)
	client.UserAgent = "dshd/" + version
	raw, found, err := llm.DiscoverEnvModelCatalogEntry(ctx, client, modelID, apiKeyEnv)
	if err != nil || !found {
		return false, err
	}
	return catalog.SetEnvModelFallback(raw)
}
