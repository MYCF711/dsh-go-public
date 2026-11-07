package transport

// 官方桌面端 pluginManager 12 个方法的契约测试。
//
// 这些测试只打本包的 `ServeOfficialPluginManagerAPI`（Lead 接线后由 /api 路由
// 调它），因此不依赖 server.go 的分发表——**路由接线是 Lead 的验收项**，测试里
// 用 IsOfficialPluginManagerMethod 钉住"方法名集合"，避免新增方法从路由漏下去。
//
// 真实状态（磁盘包、事务、启用位、安装作业）由 `internal/pluginmgr/official`
// 的适配器覆盖；本文件只钉协议：参数取名、严格性、结果形状、四字段错误。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// fakePluginManagerAPI 记录宿主适配器收到的**已解析**参数，并回放固定结果。
type fakePluginManagerAPI struct {
	seen map[string]any

	inspect        OfficialInspectParams
	listVersions   OfficialListVersionsParams
	install        OfficialInstallBundleParams
	update         OfficialUpdateBundleParams
	remove         OfficialBundleNameParams
	bundleEnabled  OfficialSetEnabledParams
	pluginEnabled  OfficialSetEnabledParams
	exemption      OfficialVersionExemptionParams
	exemptions     OfficialVersionExemptions
	detail         OfficialReadBundleDetailParams
	waitID         string
	cancelID       string
	listVersionRet OfficialVersionList
	// exemptionsRet 是 listVersionExemptions 的回放值。
	exemptionsRet OfficialVersionExemptions
}

func (f *fakePluginManagerAPI) api() OfficialPluginManagerAPI {
	record := func(name string, value any) { f.seen[name] = value }
	return OfficialPluginManagerAPI{
		Inspect: func(p OfficialInspectParams) (OfficialPluginSpecInspection, error) {
			f.inspect = p
			record("inspect", p)
			if p.Spec == "broken" {
				return OfficialInspectionRefused("not-a-package", "目录里没有 plugin.json"), nil
			}
			return OfficialInspectionAccepted("path", "acme.audit", "1.0.0", false), nil
		},
		ListVersions: func(p OfficialListVersionsParams) (OfficialVersionList, error) {
			f.listVersions = p
			record("listVersions", p)
			if f.listVersionRet.Status != "" {
				return f.listVersionRet, nil
			}
			return OfficialVersionsRefused("not-updatable", "本仓包来自本地目录，不在 registry"), nil
		},
		InstallBundle: func(p OfficialInstallBundleParams) (OfficialChangeResult, error) {
			f.install = p
			record("installBundle", p)
			return OfficialAppliedChange("install", p.Spec, true), nil
		},
		UpdateBundle: func(p OfficialUpdateBundleParams) (OfficialUpdateResult, error) {
			f.update = p
			record("updateBundle", p)
			return OfficialUpdateResult{
				From: "1.0.0", To: p.Version, Step: "install",
				Result: OfficialAppliedChange("install", p.Name, true),
			}, nil
		},
		RemoveBundle: func(p OfficialBundleNameParams) (OfficialChangeResult, error) {
			f.remove = p
			record("removeBundle", p)
			return OfficialAppliedChange("remove", p.Name, false), nil
		},
		SetBundleEnabled: func(p OfficialSetEnabledParams) (OfficialChangeResult, error) {
			f.bundleEnabled = p
			record("setBundleEnabled", p)
			c := OfficialAppliedChange("enable", p.Name, p.Enabled)
			c.Enabled = &p.Enabled
			return c, nil
		},
		SetPluginEnabled: func(p OfficialSetEnabledParams) (OfficialChangeResult, error) {
			f.pluginEnabled = p
			record("setPluginEnabled", p)
			c := OfficialAppliedChange("enable", p.Name, p.Enabled)
			c.Enabled = &p.Enabled
			return c, nil
		},
		SetVersionExemption: func(p OfficialVersionExemptionParams) (OfficialChangeResult, error) {
			f.exemption = p
			record("setVersionExemption", p)
			return OfficialAppliedChange("enable", p.PackageVersion, p.Enabled), nil
		},
		ListVersionExemptions: func() (OfficialVersionExemptions, error) {
			f.exemptions = f.exemptionsRet
			record("listVersionExemptions", f.exemptionsRet)
			return f.exemptionsRet, nil
		},
		ReadBundleDetail: func(p OfficialReadBundleDetailParams) (OfficialBundleDetail, error) {
			f.detail = p
			record("readBundleDetail", p)
			return OfficialBundleDetail{Status: "none"}, nil
		},
		WaitForInstall: func(p OfficialRequestIDParams) (OfficialInstallWait, error) {
			f.waitID = p.RequestID
			record("waitForInstall", p)
			return OfficialInstallWait{}, nil
		},
		CancelInstall: func(p OfficialRequestIDParams) (OfficialInstallCancellation, error) {
			f.cancelID = p.RequestID
			record("cancelInstall", p)
			return OfficialInstallCancellation{Status: "not-running"}, nil
		},
	}
}

func officialPluginTestServer(t *testing.T, api OfficialPluginManagerAPI) *Server {
	t.Helper()
	srv, err := NewServer(Options{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv.AttachOfficialPluginManagerAPI(api)
	t.Cleanup(srv.DetachOfficialPluginManagerAPI)
	return srv
}

// callPluginManager 打一次方法，返回 HTTP 状态与解码后的信封。
func callPluginManager(t *testing.T, srv *Server, method string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case nil:
		raw = nil
	case string:
		raw = []byte(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		raw = encoded
	}
	req := httptest.NewRequest(http.MethodPost, "/api/"+method, bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.ServeOfficialPluginManagerAPI(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s 的响应不是 JSON 对象: %q", method, rec.Body.String())
		}
	}
	return rec.Code, out
}

// resultOf 取出 {"result":…} 里的对象。
func resultOf(t *testing.T, code int, out map[string]any) map[string]any {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，期望 200（body=%v）", code, out)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("成功信封缺 result 对象：%v", out)
	}
	return result
}

// officialPluginManagerMinimalBodies 是每个方法的**最小合法请求体**。
func officialPluginManagerMinimalBodies() map[Method]any {
	return map[Method]any{
		MethodPluginManagerListVersions:     map[string]any{"name": "acme.audit"},
		MethodPluginManagerInspect:          map[string]any{"spec": "./private/v1"},
		MethodPluginManagerInstallBundle:    map[string]any{"spec": "./private/v1"},
		MethodPluginManagerUpdateBundle:     map[string]any{"name": "acme.audit", "version": "2.0.0"},
		MethodPluginManagerRemoveBundle:     map[string]any{"name": "acme.audit"},
		MethodPluginManagerSetBundleEnabled: map[string]any{"name": "acme.audit", "enabled": true},
		MethodPluginManagerSetPluginEnabled: map[string]any{"id": "acme.audit", "enabled": false},
		MethodPluginManagerSetVersionExemption: map[string]any{
			"packageVersion": "acme.audit@1.0.0", "runtimeVersion": "0.2.2-rc.1",
			"enabled": true, "acceptRisk": true,
		},
		// 官方 listVersionExemptions 是 direct 调用、零参数：空对象即最小合法体。
		MethodPluginManagerListVersionExemptions: map[string]any{},
		MethodPluginManagerReadBundleDetail:      map[string]any{"name": "acme.audit", "language": "zh-CN"},
		MethodPluginManagerWaitForInstall:        map[string]any{"requestId": "req-1"},
		MethodPluginManagerCancelInstall:         map[string]any{"requestId": "req-1"},
	}
}

func TestOfficialPluginManagerMethodsAreStable(t *testing.T) {
	methods := OfficialPluginManagerMethods()
	want := []string{
		"pluginManager.cancelInstall", "pluginManager.inspect", "pluginManager.installBundle",
		"pluginManager.listVersionExemptions", "pluginManager.listVersions",
		"pluginManager.readBundleDetail", "pluginManager.removeBundle",
		"pluginManager.setBundleEnabled", "pluginManager.setPluginEnabled",
		"pluginManager.setVersionExemption", "pluginManager.updateBundle",
		"pluginManager.waitForInstall",
	}
	if len(methods) != len(want) {
		t.Fatalf("官方 pluginManager 方法应是 %d 个，实际 %d：%v", len(want), len(methods), methods)
	}
	got := make([]string, 0, len(methods))
	seen := map[Method]bool{}
	for _, m := range methods {
		if seen[m] {
			t.Fatalf("方法重复：%s", m)
		}
		seen[m] = true
		if !IsOfficialPluginManagerMethod(string(m)) {
			t.Fatalf("%s 没被 IsOfficialPluginManagerMethod 认出来（路由会漏）", m)
		}
		if !strings.HasPrefix(string(m), "pluginManager.") {
			t.Fatalf("%s 不在 pluginManager 命名空间", m)
		}
		got = append(got, string(m))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("方法集合不一致\n got=%v\nwant=%v", got, want)
	}
	for _, other := range []string{
		"plugin.list", "pluginManager", "pluginmanager.inspect",
		"pluginManager.listBundles", "pluginManager.listPlugins",
		"pluginManager.registries", "pluginManager.listVersionExemption", "",
	} {
		if IsOfficialPluginManagerMethod(other) {
			t.Fatalf("%q 不该被当成官方 pluginManager 方法", other)
		}
	}
}

// 宿主没接线时每个方法都必须是四字段 501，并点名缺的**具体能力**。
func TestOfficialPluginManagerUnwiredIsLoudPerMethod(t *testing.T) {
	srv := officialPluginTestServer(t, OfficialPluginManagerAPI{})
	bodies := officialPluginManagerMinimalBodies()
	for _, method := range OfficialPluginManagerMethods() {
		code, out := callPluginManager(t, srv, string(method), bodies[method])
		assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
		obj := out["error"].(map[string]any)
		msg, _ := obj["message"].(string)
		if !strings.Contains(msg, string(method)) {
			t.Fatalf("%s 的 501 没点名方法：%q", method, msg)
		}
		if _, has := out["result"]; has {
			t.Fatalf("%s 的错误信封里出现了 result", method)
		}
	}
}

// 参数解析必须 fail-loud：未知字段、缺字段、错类型、options 二义都是 400。
func TestOfficialPluginManagerParamsAreStrict(t *testing.T) {
	api := &fakePluginManagerAPI{seen: map[string]any{}}
	srv := officialPluginTestServer(t, api.api())

	tests := []struct {
		name    string
		method  Method
		body    any
		message string
	}{
		{"未知字段", MethodPluginManagerRemoveBundle, map[string]any{"name": "a", "extra": 1}, "extra"},
		{"缺必填", MethodPluginManagerRemoveBundle, map[string]any{}, "name"},
		{"错类型", MethodPluginManagerRemoveBundle, map[string]any{"name": 7}, "name"},
		{"对象参数不是对象", MethodPluginManagerInspect, map[string]any{"spec": "x", "options": 3}, "options"},
		{"options 未知键", MethodPluginManagerInspect, map[string]any{"spec": "x", "options": map[string]any{"nope": true}}, "nope"},
		{"options 二义", MethodPluginManagerInstallBundle, map[string]any{
			"spec": "x", "options": map[string]any{"enabled": true}, "enabled": false,
		}, "enabled"},
		{"enabled 不是布尔", MethodPluginManagerSetBundleEnabled, map[string]any{"name": "a", "enabled": "yes"}, "enabled"},
		{"requestId 不是字符串", MethodPluginManagerWaitForInstall, map[string]any{"requestId": 1}, "requestId"},
		{"requestId 空串", MethodPluginManagerWaitForInstall, map[string]any{"requestId": "  "}, "requestId"},
		{"body 不是对象", MethodPluginManagerInspect, "[]", "JSON 对象"},
		{"listVersionExemptions 请求体为空", MethodPluginManagerListVersionExemptions, "", "请求体为空"},
		{"listVersionExemptions 不接受参数", MethodPluginManagerListVersionExemptions, map[string]any{"name": "x"}, "name"},
		{"spec 空串", MethodPluginManagerInspect, map[string]any{"spec": "   "}, "spec"},
		{"version 空串", MethodPluginManagerUpdateBundle, map[string]any{"name": "a", "version": ""}, "version"},
		{"language 形状非法", MethodPluginManagerReadBundleDetail, map[string]any{"name": "a", "language": "zh_CN"}, "language"},
		{"npmFileSources 非法值", MethodPluginManagerReadBundleDetail, map[string]any{
			"name": "a", "language": "zh-CN",
			"options": map[string]any{"npmFileSources": []string{"ftp.example"}},
		}, "npmFileSources"},
		{"packageVersion 缺 @", MethodPluginManagerSetVersionExemption, map[string]any{
			"packageVersion": "acme.audit", "runtimeVersion": "0.2.2-rc.1", "enabled": true, "acceptRisk": true,
		}, "packageVersion"},
		{"packageVersion 版本非法", MethodPluginManagerSetVersionExemption, map[string]any{
			"packageVersion": "acme.audit@1.0", "runtimeVersion": "0.2.2-rc.1", "enabled": true, "acceptRisk": true,
		}, "packageVersion"},
		{"acceptRisk 不是布尔", MethodPluginManagerSetVersionExemption, map[string]any{
			"packageVersion": "acme.audit@1.0.0", "runtimeVersion": "0.2.2-rc.1", "enabled": true, "acceptRisk": "yes",
		}, "acceptRisk"},
		{"授权豁免缺 acceptRisk", MethodPluginManagerSetVersionExemption, map[string]any{
			"packageVersion": "acme.audit@1.0.0", "runtimeVersion": "0.2.2-rc.1", "enabled": true,
		}, "acceptRisk"},
		{"撤销豁免不需要 acceptRisk", MethodPluginManagerSetVersionExemption, map[string]any{
			"packageVersion": "acme.audit@1.0.0", "runtimeVersion": "0.2.2-rc.1", "enabled": false,
		}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.message == "" {
				// 撤销豁免（enabled:false）不需要 acceptRisk：这里只要求它
				// **不**被 parameter 校验拒掉（本用例走的是已接线的假宿主）。
				if code, out := callPluginManager(t, srv, string(test.method), test.body); code != http.StatusOK {
					t.Fatalf("撤销豁免被拒了：%d %v", code, out)
				}
				return
			}
			code, out := callPluginManager(t, srv, string(test.method), test.body)
			assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
			obj := out["error"].(map[string]any)
			msg, _ := obj["message"].(string)
			if !strings.Contains(msg, test.message) {
				t.Fatalf("错误消息没点名 %q：%q", test.message, msg)
			}
		})
	}
}

// 每个方法的 wire 名都要如实进宿主：官方位置参数按名字取，不许猜。
func TestOfficialPluginManagerPassesParsedParams(t *testing.T) {
	api := &fakePluginManagerAPI{seen: map[string]any{}}
	srv := officialPluginTestServer(t, api.api())

	bodies := officialPluginManagerMinimalBodies()
	for _, method := range OfficialPluginManagerMethods() {
		if code, out := callPluginManager(t, srv, string(method), bodies[method]); code != http.StatusOK {
			t.Fatalf("%s 最小合法请求被拒：%d %v", method, code, out)
		}
	}

	if api.inspect.Spec != "./private/v1" || api.inspect.Registry != nil {
		t.Fatalf("inspect 参数不对：%+v", api.inspect)
	}
	if api.listVersions.Name != "acme.audit" || api.listVersions.Registry != nil {
		t.Fatalf("listVersions 参数不对：%+v", api.listVersions)
	}
	// installBundle 的 enabled 缺省是 nil（默认 true 由宿主语义决定，Transport 不替它编）。
	if api.install.Spec != "./private/v1" || api.install.Enabled != nil || api.install.RequestID != nil {
		t.Fatalf("installBundle 参数不对：%+v", api.install)
	}
	if api.update.Name != "acme.audit" || api.update.Version != "2.0.0" || api.update.RequestID != nil {
		t.Fatalf("updateBundle 参数不对：%+v", api.update)
	}
	if api.remove.Name != "acme.audit" {
		t.Fatalf("removeBundle 参数不对：%+v", api.remove)
	}
	if api.bundleEnabled.Name != "acme.audit" || !api.bundleEnabled.Enabled {
		t.Fatalf("setBundleEnabled 参数不对：%+v", api.bundleEnabled)
	}
	// setPluginEnabled 官方参数名是 id，不是 name。
	if api.pluginEnabled.Name != "acme.audit" || api.pluginEnabled.Enabled {
		t.Fatalf("setPluginEnabled 参数不对：%+v", api.pluginEnabled)
	}
	if api.exemption.PackageVersion != "acme.audit@1.0.0" || api.exemption.RuntimeVersion != "0.2.2-rc.1" ||
		!api.exemption.Enabled || api.exemption.AcceptRisk == nil || !*api.exemption.AcceptRisk {
		t.Fatalf("setVersionExemption 参数不对：%+v", api.exemption)
	}
	if api.detail.Name != "acme.audit" || api.detail.Language != "zh-CN" || len(api.detail.NpmFileSources) != 0 {
		t.Fatalf("readBundleDetail 参数不对：%+v", api.detail)
	}
	if api.waitID != "req-1" || api.cancelID != "req-1" {
		t.Fatalf("requestId 没进宿主：wait=%q cancel=%q", api.waitID, api.cancelID)
	}
}

// 嵌套 options 与扁平写法等价；注册表与非空构建审批要如实进宿主。
func TestOfficialPluginManagerOptionsNestingAndValues(t *testing.T) {
	api := &fakePluginManagerAPI{seen: map[string]any{}}
	srv := officialPluginTestServer(t, api.api())

	code, out := callPluginManager(t, srv, string(MethodPluginManagerInstallBundle), map[string]any{
		"spec": "C:/tmp/pkg",
		"options": map[string]any{
			"enabled": true, "saveExact": true, "requestId": "req-9",
			"approvedBuilds": []string{"esbuild"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("嵌套 options 被拒：%d %v", code, out)
	}
	got := api.install
	if got.Enabled == nil || !*got.Enabled || got.SaveExact == nil || !*got.SaveExact {
		t.Fatalf("installBundle options 丢字段：%+v", got)
	}
	if got.RequestID == nil || *got.RequestID != "req-9" {
		t.Fatalf("installBundle requestId 丢了：%+v", got)
	}
	if len(got.ApprovedBuilds) != 1 || got.ApprovedBuilds[0] != "esbuild" {
		t.Fatalf("installBundle approvedBuilds 丢了：%+v", got)
	}

	// 扁平写法同样生效。
	code, out = callPluginManager(t, srv, string(MethodPluginManagerListVersions), map[string]any{
		"name": "acme.audit", "registry": "https://registry.example",
	})
	if code != http.StatusOK {
		t.Fatalf("扁平 registry 被拒：%d %v", code, out)
	}
	if api.listVersions.Registry == nil || *api.listVersions.Registry != "https://registry.example" {
		t.Fatalf("listVersions registry 丢了：%+v", api.listVersions)
	}

	// registry: null 必须与"没给"区分开（官方是 optional 但可为 null）。
	code, out = callPluginManager(t, srv, string(MethodPluginManagerReadBundleDetail), map[string]any{
		"name": "acme.audit", "language": "en",
		"options": map[string]any{"npmFileSources": []string{"jsdelivr", "npmmirror"}},
	})
	if code != http.StatusOK {
		t.Fatalf("npmFileSources 被拒：%d %v", code, out)
	}
	if len(api.detail.NpmFileSources) != 2 || api.detail.NpmFileSources[0] != "jsdelivr" {
		t.Fatalf("npmFileSources 丢了：%+v", api.detail)
	}
}

// 结果形状逐字段钉住：客户端读的键必须存在且类型正确。
func TestOfficialPluginManagerResultShapes(t *testing.T) {
	api := &fakePluginManagerAPI{seen: map[string]any{}}
	srv := officialPluginTestServer(t, api.api())

	t.Run("inspect accepted", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerInspect), map[string]any{"spec": "./p"})
		result := resultOf(t, code, out)
		if result["status"] != "accepted" || result["kind"] != "path" {
			t.Fatalf("inspect accepted 形状不对：%v", result)
		}
		if result["bundle"] != false {
			t.Fatalf("bundle 必须是显式 false：%v", result)
		}
		if v, has := result["registry"]; !has || v != nil {
			t.Fatalf("registry 必须显式出现且为 null：%v", result)
		}
		for _, key := range []string{"problem", "reason", "host"} {
			if _, has := result[key]; has {
				t.Fatalf("accepted 不该带 %s：%v", key, result)
			}
		}
	})

	t.Run("inspect refused", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerInspect), map[string]any{"spec": "broken"})
		result := resultOf(t, code, out)
		if result["status"] != "refused" || result["problem"] != "not-a-package" {
			t.Fatalf("inspect refused 形状不对：%v", result)
		}
		if reason, _ := result["reason"].(string); reason == "" {
			t.Fatalf("refused 必须带 reason：%v", result)
		}
		for _, key := range []string{"kind", "bundle", "registry", "name"} {
			if _, has := result[key]; has {
				t.Fatalf("refused 不该带 %s：%v", key, result)
			}
		}
	})

	t.Run("listVersions refused", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerListVersions), map[string]any{"name": "acme.audit"})
		result := resultOf(t, code, out)
		if result["status"] != "refused" || result["problem"] != "not-updatable" {
			t.Fatalf("listVersions refused 形状不对：%v", result)
		}
		attempts, ok := result["attempts"].([]any)
		if !ok || len(attempts) != 0 {
			t.Fatalf("attempts 必须是空数组（不是缺键）：%v", result)
		}
		if reason, _ := result["reason"].(string); reason == "" {
			t.Fatalf("refused 必须带 reason：%v", result)
		}
	})

	t.Run("listVersions listed", func(t *testing.T) {
		api.listVersionRet = OfficialVersionList{
			Status: "listed", Installed: "1.0.0", Registry: OfficialNullableString(""),
			Attempts: []OfficialRegistryAttempt{{Registry: OfficialNullableString("")}},
			Versions: []OfficialPublishedVersion{{
				Version: "2.0.0", Tags: []string{"latest"}, Prerelease: false, Bundle: true,
			}},
			Omitted: OfficialIntPtr(0),
		}
		defer func() { api.listVersionRet = OfficialVersionList{} }()
		code, out := callPluginManager(t, srv, string(MethodPluginManagerListVersions), map[string]any{"name": "acme.audit"})
		result := resultOf(t, code, out)
		if result["status"] != "listed" || result["installed"] != "1.0.0" {
			t.Fatalf("listVersions listed 形状不对：%v", result)
		}
		if _, has := result["registry"]; !has {
			t.Fatalf("listed 必须显式带 registry（可为 null）：%v", result)
		}
		if omitted, ok := result["omitted"].(float64); !ok || omitted != 0 {
			t.Fatalf("omitted 必须是数字 0：%v", result)
		}
		versions, _ := result["versions"].([]any)
		if len(versions) != 1 {
			t.Fatalf("versions 少了：%v", result)
		}
		first := versions[0].(map[string]any)
		if first["bundle"] != true || first["prerelease"] != false {
			t.Fatalf("版本行字段不对：%v", first)
		}
		tags, _ := first["tags"].([]any)
		if len(tags) != 1 || tags[0] != "latest" {
			t.Fatalf("tags 丢了：%v", first)
		}
	})

	t.Run("change result", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerInstallBundle), map[string]any{"spec": "./p"})
		result := resultOf(t, code, out)
		for _, key := range []string{"changed", "application", "stage", "target"} {
			if _, has := result[key]; !has {
				t.Fatalf("change 结果缺 %s：%v", key, result)
			}
		}
		if result["application"] != "applied" || result["stage"] != "install" {
			t.Fatalf("change 结果值不对：%v", result)
		}
		if warnings, ok := result["warnings"].([]any); !ok || len(warnings) == 0 {
			t.Fatalf("本仓必须显式警告「没有运行时加载器」：%v", result)
		}
	})

	t.Run("update result", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerUpdateBundle),
			map[string]any{"name": "acme.audit", "version": "2.0.0"})
		result := resultOf(t, code, out)
		for _, key := range []string{"from", "to", "step", "result"} {
			if _, has := result[key]; !has {
				t.Fatalf("update 结果缺 %s：%v", key, result)
			}
		}
		inner, ok := result["result"].(map[string]any)
		if !ok || inner["stage"] != "install" {
			t.Fatalf("update.result 形状不对：%v", result)
		}
	})

	t.Run("cancel result", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerCancelInstall), map[string]any{"requestId": "r"})
		result := resultOf(t, code, out)
		if result["status"] != "not-running" {
			t.Fatalf("cancel 形状不对：%v", result)
		}
	})

	// 官方 listVersionExemptions 的两个键都是必填：空豁免/空警告也必须编成
	// `{}` / `[]`，不能缺键（客户端会把缺键当"没有这个字段"）。
	t.Run("listVersionExemptions empty", func(t *testing.T) {
		api.exemptionsRet = OfficialVersionExemptions{}
		code, out := callPluginManager(t, srv, string(MethodPluginManagerListVersionExemptions), map[string]any{})
		result := resultOf(t, code, out)
		exemptions, ok := result["exemptions"].(map[string]any)
		if !ok || len(exemptions) != 0 {
			t.Fatalf("exemptions 必须是空对象（不是缺键/null）：%v", result)
		}
		warnings, ok := result["warnings"].([]any)
		if !ok || len(warnings) != 0 {
			t.Fatalf("warnings 必须是空数组（不是缺键/null）：%v", result)
		}
	})

	t.Run("listVersionExemptions listed", func(t *testing.T) {
		api.exemptionsRet = OfficialVersionExemptions{
			Exemptions: map[string][]string{"acme.future@1.0.0": {"0.2.2-rc.1"}},
			Warnings:   []string{"compatibility.json 不是合法 JSON；按没有豁免处理"},
		}
		defer func() { api.exemptionsRet = OfficialVersionExemptions{} }()
		code, out := callPluginManager(t, srv, string(MethodPluginManagerListVersionExemptions), map[string]any{})
		result := resultOf(t, code, out)
		exemptions, _ := result["exemptions"].(map[string]any)
		versions, _ := exemptions["acme.future@1.0.0"].([]any)
		if len(versions) != 1 || versions[0] != "0.2.2-rc.1" {
			t.Fatalf("exemptions 键/版本丢了：%v", result)
		}
		warnings, _ := result["warnings"].([]any)
		if len(warnings) != 1 {
			t.Fatalf("warnings 必须原样带出：%v", result)
		}
	})

	t.Run("wait result", func(t *testing.T) {
		code, out := callPluginManager(t, srv, string(MethodPluginManagerWaitForInstall), map[string]any{"requestId": "r"})
		result := resultOf(t, code, out)
		// 官方的 `null` 在本仓不能写成 {"result":null}（出口会变 500）：
		// 用 active:false 表达"没有在飞作业"，见 RESULT 的 wire 差异。
		if result["active"] != false {
			t.Fatalf("wait 形状不对：%v", result)
		}
		if _, has := result["result"]; has {
			t.Fatalf("active:false 不该带 result：%v", result)
		}
	})
}

// 两个 Server 各自挂在自家的宿主上，A/B 不串。
func TestOfficialPluginManagerMountIsPerServer(t *testing.T) {
	first := &fakePluginManagerAPI{seen: map[string]any{}}
	second := &fakePluginManagerAPI{seen: map[string]any{}}
	a := officialPluginTestServer(t, first.api())
	b := officialPluginTestServer(t, second.api())

	if code, out := callPluginManager(t, a, string(MethodPluginManagerRemoveBundle), map[string]any{"name": "A"}); code != http.StatusOK {
		t.Fatalf("A 调用失败：%d %v", code, out)
	}
	if code, out := callPluginManager(t, b, string(MethodPluginManagerRemoveBundle), map[string]any{"name": "B"}); code != http.StatusOK {
		t.Fatalf("B 调用失败：%d %v", code, out)
	}
	if first.remove.Name != "A" || second.remove.Name != "B" {
		t.Fatalf("A/B 串了：a=%q b=%q", first.remove.Name, second.remove.Name)
	}

	b.DetachOfficialPluginManagerAPI()
	code, out := callPluginManager(t, b, string(MethodPluginManagerRemoveBundle), map[string]any{"name": "B"})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
}

// 宿主返回的错误必须原样走四字段（含 not_implemented / not_found / bad_request）。
func TestOfficialPluginManagerHostErrorsStayFourField(t *testing.T) {
	api := OfficialPluginManagerAPI{
		Inspect: func(OfficialInspectParams) (OfficialPluginSpecInspection, error) {
			return OfficialPluginSpecInspection{}, fmt.Errorf("未接线：%w", ErrOfficialNotImplemented)
		},
		ReadBundleDetail: func(OfficialReadBundleDetailParams) (OfficialBundleDetail, error) {
			return OfficialBundleDetail{}, officialNotFound("没有这个 bundle")
		},
		RemoveBundle: func(OfficialBundleNameParams) (OfficialChangeResult, error) {
			return OfficialChangeResult{}, officialBadRequest("name 非法")
		},
	}
	srv := officialPluginTestServer(t, api)
	code, out := callPluginManager(t, srv, string(MethodPluginManagerInspect), map[string]any{"spec": "x"})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	code, out = callPluginManager(t, srv, string(MethodPluginManagerReadBundleDetail),
		map[string]any{"name": "x", "language": "en"})
	assertFourFieldError(t, code, out, http.StatusNotFound, "not_found")
	code, out = callPluginManager(t, srv, string(MethodPluginManagerRemoveBundle), map[string]any{"name": "x"})
	assertFourFieldError(t, code, out, http.StatusBadRequest, "bad_request")
}

// 官方第 12 个 pluginManager 方法 `listVersionExemptions()`：direct 调用、
// 零参数，结果是 `{exemptions: Record<string,string[]>, warnings: string[]}`。
//
// 先红：它当前**没登记**（不在 OfficialPluginManagerMethods 里，路由会把它当
// 未知路径回 404），所以这里两条断言都会先打在"未登记"上。
func TestOfficialPluginManagerListVersionExemptionsIsRegistered(t *testing.T) {
	const method = "pluginManager.listVersionExemptions"
	if !IsOfficialPluginManagerMethod(method) {
		t.Fatalf("%s 没被登记：官方 153 方法里 pluginManager 的第 12 个被漏掉了（路由会回 404 not_found）", method)
	}
	srv := officialPluginTestServer(t, OfficialPluginManagerAPI{})
	code, out := callPluginManager(t, srv, method, map[string]any{})
	assertFourFieldError(t, code, out, http.StatusNotImplemented, "not_implemented")
	obj := out["error"].(map[string]any)
	if msg, _ := obj["message"].(string); !strings.Contains(msg, method) {
		t.Fatalf("%s 的 501 没点名方法：%q", method, msg)
	}
}
