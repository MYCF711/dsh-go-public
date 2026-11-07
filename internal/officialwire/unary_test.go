package officialwire

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// unaryBindings 是 unary 测试用的方法表。
func unaryBindings() map[string]Binding {
	return map[string]Binding{
		// 有 descriptor 的成功方法。
		"session/page": {
			Descriptor: &Descriptor{Required: []string{"address"}, Optional: []string{"maxMessages"}},
			Handle: func(_ context.Context, args map[string]json.RawMessage) (any, *Error) {
				if _, ok := args["maxMessages"]; ok {
					return map[string]any{"cursor": 9.0, "records": []any{}, "hasMore": true}, nil
				}
				return map[string]any{"cursor": 7.0, "records": []any{}, "hasMore": false}, nil
			},
		},
		// 业务失败。
		"session/notFoundCase": {
			Handle: func(context.Context, map[string]json.RawMessage) (any, *Error) {
				return nil, NewError("session/not-found", "没有这个会话", map[string]any{"sessionId": "x"})
			},
		},
		// 处理器 panic：必须变成 500，而不是掐掉连接。
		"session/panicCase": {
			Handle: func(context.Context, map[string]json.RawMessage) (any, *Error) {
				panic("处理器内部炸了")
			},
		},
		// 纯二进制结果（走 multipart）。
		"workspaceFiles/readBytes": {
			Descriptor: &Descriptor{Required: []string{"path"}},
			Handle: func(context.Context, map[string]json.RawMessage) (any, *Error) {
				return map[string]any{
					"offset":       0.0,
					"data":         []byte("hello-bytes"),
					"eof":          true,
					"absolutePath": "/w/a.bin",
					"version":      "v1",
				}, nil
			},
		},
	}
}

// TestUnarySuccessEnvelopeAndRPCIDEcho 钉住成功信封与 rpcId 回显（EVIDENCE §2.2）。
func TestUnarySuccessEnvelopeAndRPCIDEcho(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	rpcID := "9f1c-逐字-回显-😀"
	status, payload := postEnvelope(t, ts, client, "session/page", envelopeBody(rpcID, "session/page", `{"address":{"kind":"session","sessionId":"s"}}`))
	envelope := decodeEnvelope(t, status, payload)
	if envelope["rpcId"] != rpcID {
		t.Fatalf("rpcId 必须逐字回显：got=%v want=%q 原文=%s", envelope["rpcId"], rpcID, payload)
	}
	result := resultOf(t, envelope)
	if ok, _ := result["ok"].(bool); !ok {
		t.Fatalf("result.ok 应为 true：%v", envelope)
	}
	value, ok := result["value"].(map[string]any)
	if !ok {
		t.Fatalf("result.value 应是对象：%v", result)
	}
	if value["cursor"] != 7.0 {
		t.Fatalf("value 不透传：%v", value)
	}
}

// TestUnaryBusinessFailureIsHTTP200 钉住 200-on-error（EVIDENCE §2.2、§6）。
func TestUnaryBusinessFailureIsHTTP200(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	status, payload := postEnvelope(t, ts, client, "session/notFoundCase", envelopeBody("r-fail", "session/notFoundCase", `{}`))
	envelope := decodeEnvelope(t, status, payload) // 内部已断言 status==200
	failure := failureOf(t, envelope)
	if failure["code"] != "session/not-found" {
		t.Fatalf("业务错误码必须原样透出：%v", failure)
	}
	details, _ := failure["details"].(map[string]any)
	if details["sessionId"] != "x" {
		t.Fatalf("details 必须透出：%v", details)
	}
}

// TestUnaryUnknownMethodIsHTTP200UnknownMethod 钉住未知方法的分支。
//
// 它与"已接线但不可用"必须区分：前者是客户端拼错，后者是宿主缺能力。
func TestUnaryUnknownMethodIsHTTP200UnknownMethod(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	status, payload := postEnvelope(t, ts, client, "session/nope", envelopeBody("r-unknown", "session/nope", `{}`))
	envelope := decodeEnvelope(t, status, payload)
	failure := failureOf(t, envelope)
	if failure["code"] != CodeUnknownMethod {
		t.Fatalf("未知方法应回 %s，实际 %v", CodeUnknownMethod, failure)
	}
	if envelope["rpcId"] != "r-unknown" {
		t.Fatalf("未知方法也要回显 rpcId：%v", envelope)
	}
}

// TestUnaryArgumentsInvalidIsHTTP200 钉住"多字段/缺字段 fail-loud 且仍是 200"。
func TestUnaryArgumentsInvalidIsHTTP200(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	cases := []struct {
		name string
		args string
		field string
	}{
		{"多一个字段", `{"address":{"kind":"session","sessionId":"s"},"bogus":1}`, "bogus"},
		{"缺必填字段", `{"maxMessages":5}`, "address"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, payload := postEnvelope(t, ts, client, "session/page",
				envelopeBody("r-args", "session/page", testCase.args))
			envelope := decodeEnvelope(t, status, payload)
			failure := failureOf(t, envelope)
			if failure["code"] != CodeArgumentsInvalid {
				t.Fatalf("应回 %s，实际 %v", CodeArgumentsInvalid, failure)
			}
			details, _ := failure["details"].(map[string]any)
			if details["field"] != testCase.field {
				t.Fatalf("details.field 应指向 %q：%v", testCase.field, details)
			}
			if envelope["rpcId"] != "r-args" {
				t.Fatalf("参数错误也要回显 rpcId：%v", envelope)
			}
		})
	}
}

// TestUnaryEnvelopeMethodMismatchIsHTTP200 钉住 body.method 与 URL 不一致的分支。
func TestUnaryEnvelopeMethodMismatchIsHTTP200(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	status, payload := postEnvelope(t, ts, client, "session/page",
		envelopeBody("r-mismatch", "session/events", `{"address":{"kind":"session","sessionId":"s"}}`))
	envelope := decodeEnvelope(t, status, payload)
	failure := failureOf(t, envelope)
	if failure["code"] != CodeBadRequest {
		t.Fatalf("method 不一致应回 %s，实际 %v", CodeBadRequest, failure)
	}
}

// TestUnaryHandlerPanicIsHTTP500 钉住"处理器异常才 5xx，且 panic 不外泄"。
func TestUnaryHandlerPanicIsHTTP500(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	response, err := client.Post(ts.URL+"/api/session/panicCase", "application/json",
		strings.NewReader(envelopeBody("r-panic", "session/panicCase", `{}`)))
	if err != nil {
		t.Fatalf("panic 把连接掐掉了（这正是要避免的）：%v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("处理器 panic 应 500，实际 %d", response.StatusCode)
	}
	// 服务器还活着：再打一条正常方法必须成功。
	status, payload := postEnvelope(t, ts, client, "session/page",
		envelopeBody("r-after-panic", "session/page", `{"address":{"kind":"session","sessionId":"s"}}`))
	decodeEnvelope(t, status, payload)
}

// TestUnaryTransportStatuses 钉住传输层的状态码分工（EVIDENCE §6）。
func TestUnaryTransportStatuses(t *testing.T) {
	cfg := testServerConfig()
	cfg.MaxBodyBytes = 64
	_, ts, client := startWire(t, cfg, unaryBindings())

	// 未认证 → 401（先测这条，再换 cookie）。
	response, err := client.Post(ts.URL+"/api/session/page", "application/json",
		strings.NewReader(envelopeBody("r-401", "session/page", `{}`)))
	if err != nil {
		t.Fatalf("POST 未认证: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", response.StatusCode)
	}

	connectAndCookies(t, ts, client, InteractionsNone)

	// 非 POST → 404。
	response, err = client.Get(ts.URL + "/api/session/page")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("非 POST 应 404，实际 %d", response.StatusCode)
	}

	// content-type 不是 JSON → 415。
	response, err = client.Post(ts.URL+"/api/session/page", "text/plain",
		strings.NewReader(envelopeBody("r-415", "session/page", `{}`)))
	if err != nil {
		t.Fatalf("POST text/plain: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON content-type 应 415，实际 %d", response.StatusCode)
	}

	// body 不是 JSON → 400。
	response, err = client.Post(ts.URL+"/api/session/page", "application/json", strings.NewReader("{nope"))
	if err != nil {
		t.Fatalf("POST 坏 JSON: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏 JSON 应 400，实际 %d", response.StatusCode)
	}

	// body 超限 → 413。
	big := strings.Repeat("x", 4096)
	response, err = client.Post(ts.URL+"/api/session/page", "application/json",
		strings.NewReader(`{"type":"client-request","rpcId":"`+big+`","method":"session/page","payload":{"args":{}}}`))
	if err != nil {
		t.Fatalf("POST 超限: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，实际 %d", response.StatusCode)
	}
}

// TestUnaryContentTypeWithCharsetAccepted 钉住 `application/json; charset=utf-8`
// 也算 application/json（否则每一个正常客户端都会被 415）。
func TestUnaryContentTypeWithCharsetAccepted(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	response, err := client.Post(ts.URL+"/api/session/page", "application/json; charset=utf-8",
		strings.NewReader(envelopeBody("r-charset", "session/page", `{"address":{"kind":"session","sessionId":"s"}}`)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("带 charset 的 JSON 应 200，实际 %d", response.StatusCode)
	}
}

// TestUnaryBinaryResultUsesMultipart 是 unary + 附件编码的端到端证明。
func TestUnaryBinaryResultUsesMultipart(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), unaryBindings())
	connectAndCookies(t, ts, client, InteractionsNone)

	response, err := client.Post(ts.URL+"/api/workspaceFiles/readBytes", "application/json",
		strings.NewReader(envelopeBody("r-bytes", "workspaceFiles/readBytes", `{"path":"a.bin"}`)))
	if err != nil {
		t.Fatalf("POST readBytes: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("应 200，实际 %d", response.StatusCode)
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("有二进制的结果必须走 multipart，实际 %q", contentType)
	}
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, readErr := response.Body.Read(buf)
		body.Write(buf[:n])
		if readErr != nil {
			break
		}
	}
	envelope, err := DecodeMultipartResponse(contentType, []byte(body.String()))
	if err != nil {
		t.Fatalf("官方 multipart 解码失败：%v", err)
	}
	result, _ := envelope["result"].(map[string]any)
	value, _ := result["value"].(map[string]any)
	data, ok := value["data"].([]byte)
	if !ok {
		t.Fatalf("data 应被还原成字节，实际 %T（%v）", value["data"], value["data"])
	}
	if string(data) != "hello-bytes" {
		t.Fatalf("字节内容不对：%q", data)
	}
	if value["version"] != "v1" || value["absolutePath"] != "/w/a.bin" {
		t.Fatalf("其余字段必须原样保留：%v", value)
	}
}
