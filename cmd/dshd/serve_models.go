package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"dshgo/internal/llm"
	"dshgo/internal/settingsform"
)

// modelCatalogParams 是五个方法的裸参数并集；每个方法另有严格白名单。
// credential 只能写入/测试，绝不出现在任何 result 中。
type modelCatalogParams struct {
	Model            json.RawMessage `json:"model,omitempty"`
	Credential       *string         `json:"credential,omitempty"`
	ExpectedRevision *int64          `json:"expectedRevision,omitempty"`
	IDs              *[]string       `json:"ids,omitempty"`
	ID               string          `json:"id,omitempty"`
	BaseURL          *string         `json:"baseURL,omitempty"`
	API              *string         `json:"api,omitempty"`
}

var modelCatalogMethodParams = map[string][]string{
	"models.catalog.list":       {},
	"models.catalog.upsert":     {"model", "credential", "expectedRevision"},
	"models.catalog.reorder":    {"ids", "expectedRevision"},
	"models.catalog.test":       {"id", "baseURL", "api", "credential"},
	"models.catalog.set-active": {"id", "expectedRevision"},
}

type modelCatalogActive struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type modelCatalogListResult struct {
	Revision    int64                                       `json:"revision"`
	Models      []json.RawMessage                           `json:"models"`
	Credentials map[string]llm.ModelCatalogCredentialStatus `json:"credentials"`
	Active      modelCatalogActive                          `json:"active"`
}

type modelCatalogActiveResult struct {
	ActiveModel    string `json:"activeModel"`
	ActiveRevision int64  `json:"activeRevision"`
	Revision       int64  `json:"revision"`
}

func newModelCatalogHTTPHandler(catalog *llm.ModelCatalogStore, settings *serveSettingsStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		allowed, ok := modelCatalogMethodParams[method]
		if r.Method != http.MethodPost || !ok || "/api/"+method != r.URL.Path {
			writeModelCatalogError(w, modelCatalogAPIError("not_found", "未知模型目录 Transport 方法"))
			return
		}
		if catalog == nil || settings == nil {
			writeModelCatalogError(w, modelCatalogAPIError("internal", "模型目录或 W61 默认模型存储未接线"))
			return
		}
		var params modelCatalogParams
		if err := decodeModelCatalogParams(r, method, allowed, &params); err != nil {
			writeModelCatalogError(w, err)
			return
		}
		if params.ExpectedRevision != nil && *params.ExpectedRevision < 0 {
			writeModelCatalogError(w, modelCatalogAPIError("bad_request", "expectedRevision 不能为负数"))
			return
		}

		switch method {
		case "models.catalog.list":
			view, err := catalog.List()
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			result, err := makeModelCatalogListResult(view, settings)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: result})
		case "models.catalog.upsert":
			if len(params.Model) == 0 {
				writeModelCatalogError(w, modelCatalogAPIError("bad_request", "models.catalog.upsert 缺 model object"))
				return
			}
			view, err := catalog.Upsert(params.Model, params.Credential, params.ExpectedRevision)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			result, err := makeModelCatalogListResult(view, settings)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: result})
		case "models.catalog.reorder":
			if params.IDs == nil {
				writeModelCatalogError(w, modelCatalogAPIError("bad_request", "models.catalog.reorder 缺 ids 数组或 ids=null"))
				return
			}
			view, err := catalog.Reorder(*params.IDs, params.ExpectedRevision)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			result, err := makeModelCatalogListResult(view, settings)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: result})
		case "models.catalog.test":
			if err := validateModelCatalogTransportID(params.ID); err != nil {
				writeModelCatalogError(w, err)
				return
			}
			result, err := catalog.Test(r.Context(), params.ID, llm.ModelTestOverride{BaseURL: params.BaseURL, API: params.API, Credential: params.Credential})
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: result})
		case "models.catalog.set-active":
			if err := validateModelCatalogTransportID(params.ID); err != nil {
				writeModelCatalogError(w, err)
				return
			}
			view, err := catalog.List()
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			found := false
			for _, raw := range view.Models {
				var model struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &model) == nil && model.ID == params.ID {
					found = true
					break
				}
			}
			if !found {
				writeModelCatalogError(w, modelCatalogAPIError("not_found", fmt.Sprintf("模型目录没有 id %q", params.ID)))
				return
			}
			active, err := setModelCatalogActive(settings, params.ID, params.ExpectedRevision)
			if err != nil {
				writeModelCatalogError(w, err)
				return
			}
			writeSettingsFormJSON(w, http.StatusOK, settingsEnvelope{Result: modelCatalogActiveResult{
				ActiveModel: active.ID, ActiveRevision: active.Revision, Revision: view.Revision,
			}})
		}
	})
}

func makeModelCatalogListResult(view llm.ModelCatalogView, settings *serveSettingsStore) (modelCatalogListResult, error) {
	active, err := readModelCatalogActive(settings)
	if err != nil {
		return modelCatalogListResult{}, err
	}
	return modelCatalogListResult{
		Revision: view.Revision, Models: view.Models, Credentials: view.Credentials, Active: active,
	}, nil
}

func readModelCatalogActive(settings *serveSettingsStore) (modelCatalogActive, error) {
	doc, err := settings.Read(settingsform.NamespaceModel)
	if err != nil {
		return modelCatalogActive{}, err
	}
	return modelCatalogActive{ID: effectiveSettingsValue(doc, settingsform.FieldDefaultModel), Revision: doc.Revision}, nil
}

// setModelCatalogActive 在 W61 同一把 store 锁内检查 revision、写默认并构造
// 响应。catalog 不保存 active；旧会话 override 也不在这里触碰。
func setModelCatalogActive(settings *serveSettingsStore, id string, expected *int64) (modelCatalogActive, error) {
	if settings.models == nil {
		return modelCatalogActive{}, modelCatalogAPIError("internal", "W61 model override store 未接线")
	}
	settings.models.mu.Lock()
	defer settings.models.mu.Unlock()
	if settings.modelLoadErr != nil {
		return modelCatalogActive{}, fmt.Errorf("既有 model 设置存储读取失败，拒绝 set-active: %w", settings.modelLoadErr)
	}
	before, err := readSettingsDocumentLocked(settings.models.m, settings.baseModel, settingsform.FieldDefaultModel)
	if err != nil {
		return modelCatalogActive{}, err
	}
	if expected != nil && *expected != before.Revision {
		return modelCatalogActive{}, settingsform.RevisionConflict(settingsform.NamespaceModel, *expected, before.Revision)
	}
	revision, err := writeSettingsDocumentLocked(&settings.models.m,
		map[string]any{settingsform.FieldDefaultModel: id}, expected,
		settingsform.NamespaceModel, settingsform.FieldDefaultModel, settings.models.flushLocked)
	if err != nil {
		return modelCatalogActive{}, err
	}
	return modelCatalogActive{ID: id, Revision: revision}, nil
}

func validateModelCatalogTransportID(id string) error {
	if id == "" || id != strings.TrimSpace(id) {
		return modelCatalogAPIError("invalid_id", "id 必须是非空且首尾无空白的模型 id")
	}
	return nil
}

func decodeModelCatalogParams(r *http.Request, method string, allowed []string, target *modelCatalogParams) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return modelCatalogAPIError("bad_request", "读取模型目录参数失败: "+err.Error())
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return modelCatalogAPIError("bad_request", method+" 参数必须是 JSON 裸对象，不要包 params 或发送数组/null")
	}
	if _, wrapped := fields["params"]; wrapped {
		return modelCatalogAPIError("bad_request", method+" 不接受 params 包装")
	}
	whitelist := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		whitelist[key] = true
	}
	for key, value := range fields {
		if !whitelist[key] {
			return modelCatalogAPIError("bad_request", method+" 不接受参数 "+key)
		}
		if (key == "credential" || key == "baseURL" || key == "api" || key == "ids" || key == "expectedRevision") && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return modelCatalogAPIError("bad_request", method+" 的 "+key+" 不能为 null；可省略字段表示未提供")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return modelCatalogAPIError("bad_request", method+" 参数类型错误: "+err.Error())
	}
	return nil
}

func modelCatalogAPIError(code, message string) error {
	return &llm.ModelCatalogError{Code: code, Message: message, Retryable: code == "internal" || code == "upstream_error"}
}

func writeModelCatalogError(w http.ResponseWriter, err error) {
	var catalogErr *llm.ModelCatalogError
	if errors.As(err, &catalogErr) {
		status := modelCatalogHTTPStatus(catalogErr.Code)
		writeSettingsFormJSON(w, status, settingsEnvelope{Error: &settingsform.Error{
			Code: catalogErr.Code, Message: catalogErr.Message,
			Hint: modelCatalogErrorHint(catalogErr.Code), Retryable: catalogErr.Retryable,
		}})
		return
	}
	apiErr := settingsform.AsError(err)
	if apiErr.Hint == "" {
		apiErr.Hint = modelCatalogErrorHint(apiErr.Code)
	}
	status := apiErr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeSettingsFormJSON(w, status, settingsEnvelope{Error: apiErr})
}

func modelCatalogHTTPStatus(code string) int {
	switch code {
	case "bad_request", "invalid_id", "invalid_field", "invalid_config", "credential_missing", "empty_order":
		return http.StatusBadRequest
	case "not_found":
		return http.StatusNotFound
	case "empty_catalog", "duplicate_id", "revision_conflict":
		return http.StatusConflict
	case "upstream_error":
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func modelCatalogErrorHint(code string) string {
	switch code {
	case "bad_request":
		return "按 model-catalog.snapshot.json 发送该方法的裸参数与完整 ids 排列"
	case "invalid_id":
		return "使用目录中首尾无空白、非空且不超过 256 字节的模型 id"
	case "invalid_field":
		return "修正官方模型字段类型/取值；secret 只放顶层 credential，未知普通字段会保留"
	case "invalid_config":
		return "补齐 http(s) baseURL 与 openai-completions api 后重试"
	case "credential_missing":
		return "先 upsert 顶层 credential，或配置 apiKeyEnv 对应的环境变量"
	case "empty_catalog":
		return "先 upsert 至少一个模型；空目录只允许 list 与首次新增"
	case "empty_order":
		return "ids 必须包含目录全部模型，不能发送空数组"
	case "duplicate_id":
		return "每个模型 id 只能出现一次；修复重复项后重试"
	case "revision_conflict":
		return "重新 models.catalog.list，基于最新 revision 再提交"
	case "not_found":
		return "先 models.catalog.list 核对真实 id"
	case "upstream_error":
		return "这是本地端点/协议的真实失败；检查 OpenAI 兼容端点、模型 id 与 credential"
	default:
		return "检查 state/model-catalog.json 与 W61 session-model.json；失败未覆盖旧值"
	}
}
