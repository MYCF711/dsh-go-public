package officialwire

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeUploadSink 记录收到的裸字节，并回一段固定字节。
type fakeUploadSink struct {
	received RawUploadRequest
	response RawUploadResponse
	failure  *Error
}

func (s *fakeUploadSink) Upload(_ context.Context, request RawUploadRequest) (RawUploadResponse, *Error) {
	s.received = request
	if s.failure != nil {
		return RawUploadResponse{}, s.failure
	}
	return s.response, nil
}

// TestRawUploadWithoutSinkIsExplicitlyUnimplemented 钉住"明确未实现"。
func TestRawUploadWithoutSinkIsExplicitlyUnimplemented(t *testing.T) {
	_, ts, client := startWire(t, testServerConfig(), nil)
	connectAndCookies(t, ts, client, InteractionsNone)

	response, err := client.Post(ts.URL+UploadPath, "application/octet-stream", bytes.NewReader([]byte("bytes")))
	if err != nil {
		t.Fatalf("POST %s: %v", UploadPath, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotImplemented {
		t.Fatalf("没有 sink 必须 501（不是静默成功、也不是 404），实际 %d", response.StatusCode)
	}
	var failure map[string]any
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatalf("501 正文应是 JSON 错误：%v", err)
	}
	if failure["code"] != CodeMethodUnavailable {
		t.Fatalf("码应为 %s，实际 %v", CodeMethodUnavailable, failure)
	}
}

// TestRawUploadRoundTripsBinaryBytes 钉住上行裸字节是**原样**的（不做 base64/JSON）。
func TestRawUploadRoundTripsBinaryBytes(t *testing.T) {
	sink := &fakeUploadSink{response: RawUploadResponse{
		ContentType: "application/json; charset=utf-8",
		Body:        []byte(`{"attachmentId":"att-1"}`),
	}}
	cfg := testServerConfig()
	cfg.RawUpload = sink
	_, ts, client := startWire(t, cfg, nil)
	connectAndCookies(t, ts, client, InteractionsNone)

	content := []byte{0x00, 0x01, 0xff, 0xfe, 'a', '\n', 0x00}
	request, err := http.NewRequest(http.MethodPost, ts.URL+UploadPath+"?name=blob%2Ebin&sessionId=s-1", EncodeRawUpload(content))
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("应 200，实际 %d", response.StatusCode)
	}
	if !bytes.Equal(sink.received.Body, content) {
		t.Fatalf("sink 收到的字节被改写了：%v", sink.received.Body)
	}
	if sink.received.Name != "blob.bin" || sink.received.SessionID != "s-1" {
		t.Fatalf("查询参数没解出来：%+v", sink.received)
	}
	if sink.received.ContentType != "application/octet-stream" {
		t.Fatalf("content-type 没透传：%q", sink.received.ContentType)
	}
	echoed, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应: %v", err)
	}
	if string(echoed) != `{"attachmentId":"att-1"}` {
		t.Fatalf("响应字节必须原样回出：%q", echoed)
	}
}

// TestRawUploadSinkFailureIsHTTP200 钉住"业务失败也是 200"在这一层同样成立。
func TestRawUploadSinkFailureIsHTTP200(t *testing.T) {
	sink := &fakeUploadSink{failure: NewError("session/attachment-invalid", "太大", map[string]any{"limit": 8})}
	cfg := testServerConfig()
	cfg.RawUpload = sink
	_, ts, client := startWire(t, cfg, nil)
	connectAndCookies(t, ts, client, InteractionsNone)

	response, err := client.Post(ts.URL+UploadPath, "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("业务失败必须 200，实际 %d", response.StatusCode)
	}
	var envelope map[string]any
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("正文不是 JSON：%v", err)
	}
	result, _ := envelope["result"].(map[string]any)
	if ok, _ := result["ok"].(bool); ok {
		t.Fatalf("应 ok:false：%v", envelope)
	}
}

// TestRawUploadTransportGuards 钉住认证、方法、上限三条传输层判据。
func TestRawUploadTransportGuards(t *testing.T) {
	cfg := testServerConfig()
	cfg.MaxUploadBytes = 16
	cfg.RawUpload = &fakeUploadSink{response: RawUploadResponse{Body: []byte("{}")}}
	_, ts, client := startWire(t, cfg, nil)

	// 未认证 → 401。
	response, err := client.Post(ts.URL+UploadPath, "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证应 401，实际 %d", response.StatusCode)
	}

	connectAndCookies(t, ts, client, InteractionsNone)

	// 非 POST → 404。
	response, err = client.Get(ts.URL + UploadPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("非 POST 应 404，实际 %d", response.StatusCode)
	}

	// 超限 → 413。
	response, err = client.Post(ts.URL+UploadPath, "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte("x"), 1024)))
	if err != nil {
		t.Fatalf("POST 超限: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，实际 %d", response.StatusCode)
	}
}
