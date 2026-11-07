package transport

import (
	"encoding/json"
	"net/http"
)

// 界面配置在两个 handler 里只被搬动，不被解释。
//
// 协议层认的形状只有一种：**扁平的 key → string 表**。不做嵌套、不做类型推断、
// 不做合并策略——落盘位置与格式由宿主决定（ConfigGet/ConfigSet 背后是 JSON、
// TOML 还是注册表，这一层不知道也不该知道）。一旦这里开始"顺手"重新解读某个
// key，配置就有了两份语义，两边迟早对不上。

// handleConfigGet 是 /api/config.get：把宿主的整份配置原样交出去。
//
// 请求体不用读（界面调它时不带内容），所以这里不解析、也不因为"没 body"报错。
//
// 三种结果分开说：
//   - 没接线 → 501（要宿主补实现，重试没有意义）
//   - 宿主报错 → 500 internal，带上宿主原文（"读不动"和"是空的"必须能分辨）
//   - 成功 → 200；没配置过给空对象 {}，不给 null
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.opt.ConfigGet == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("config.get 未接线"))
		return
	}

	values, err := s.opt.ConfigGet()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "读配置失败："+err.Error()))
		return
	}

	// nil map 会被 encoding/json 编成 null，界面那边就得同时处理 null 和 {}。
	// 空配置在这里归一成空对象，让界面只认一种形状。
	if values == nil {
		values = map[string]string{}
	}

	// 两个键都给：`values` 是契约里的规范形状，`config` 是早先界面读的那个名字。
	// 只给一个的后果是**静默**的——界面拿到空配置，界面上什么都是默认值，
	// 而两边都不报错（界面审计实测：宿主回 values、界面读 config）。
	writeJSON(w, http.StatusOK, map[string]any{"values": values, "config": values})
}

// handleConfigSet 是 /api/config.set：只覆盖请求里给出的 key。
//
// 请求体是 {"values":{"theme":"dark","lang":"zh"}}。没给出的 key 必须保持原样——
// 这是"改若干项"而不是"整份替换"，所以这里不做"清空后重写"。
//
// 四种结果分开说：
//   - 没接线 → 501
//   - 请求体不是 JSON 对象 → 400 bad_request
//   - values 为 nil 或空 → 400 bad_request（一次都不改的空调用是调用方的 bug，
//     不该默默成功）
//   - 宿主报错 → 500 internal，带上宿主原文
//
// 成功回 updated=len(values)，让界面能对上"我提交了 3 项，改掉的是不是 3 项"。
func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if s.opt.ConfigSet == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody("config.set 未接线"))
		return
	}

	// 两种形状都收：`{"values":{…}}`（契约里的规范形状）与扁平 kv（早先界面的发法）。
	// 只认一种的后果是 400，而调用方以为自己传对了。
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "请求体不是 JSON 对象"))
		return
	}
	values := map[string]string{}
	if inner, ok := raw["values"]; ok {
		if err := json.Unmarshal(inner, &values); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "values 必须是字符串到字符串的对象"))
			return
		}
	} else {
		// 扁平形状：整份 body 就是 kv。只收字符串值。
		for k, v := range raw {
			var sv string
			if err := json.Unmarshal(v, &sv); err != nil {
				continue // 非字符串的键不是配置项，忽略
			}
			values[k] = sv
		}
	}
	// nil 与空区分不够：两者都是"这一趟什么都没说要改"，一起拦下。
	if len(values) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBodyFor("bad_request", "缺 values：要给至少一项"))
		return
	}
	req := struct{ Values map[string]string }{Values: values}

	if err := s.opt.ConfigSet(req.Values); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBodyFor("internal", "写配置失败："+err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"updated": len(req.Values)})
}
