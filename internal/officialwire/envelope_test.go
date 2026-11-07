package officialwire

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseClientRequestExactShape 钉住请求信封的 exact-key 语义（EVIDENCE §2.1）。
func TestParseClientRequestExactShape(t *testing.T) {
	valid := `{"type":"client-request","rpcId":"r-1","method":"session/page","payload":{"args":{}}}`
	request, failure := ParseClientRequest("session/page", []byte(valid))
	if failure != nil {
		t.Fatalf("合法信封不该报错：%v", failure)
	}
	if request.RPCID != "r-1" || request.Method != "session/page" || request.Endpoint != "session/page" {
		t.Fatalf("解析结果不对：%+v", request)
	}
	if len(request.Args) != 0 {
		t.Fatalf("空 args 应该解出空表：%v", request.Args)
	}

	cases := []struct {
		name   string
		body   string
		field  string
		rpcID  string
		noReq  bool
		expect string
	}{
		{
			name:  "多一个顶层字段",
			body:  `{"type":"client-request","rpcId":"r-2","method":"session/page","payload":{"args":{}},"extra":1}`,
			field: "extra", rpcID: "r-2",
		},
		{
			name:  "缺 type",
			body:  `{"rpcId":"r-3","method":"session/page","payload":{"args":{}}}`,
			field: "type", rpcID: "r-3",
		},
		{
			name:  "type 不是 client-request",
			body:  `{"type":"client-request2","rpcId":"r-4","method":"session/page","payload":{"args":{}}}`,
			field: "type", rpcID: "r-4",
		},
		{
			name:  "rpcId 不是字符串",
			body:  `{"type":"client-request","rpcId":42,"method":"session/page","payload":{"args":{}}}`,
			field: "rpcId",
		},
		{
			name:  "method 与 URL 不一致",
			body:  `{"type":"client-request","rpcId":"r-5","method":"session/events","payload":{"args":{}}}`,
			field: "method", rpcID: "r-5",
		},
		{
			name:  "payload 多一个字段",
			body:  `{"type":"client-request","rpcId":"r-6","method":"session/page","payload":{"args":{},"other":1}}`,
			field: "payload.other", rpcID: "r-6",
		},
		{
			name:  "payload 缺 args",
			body:  `{"type":"client-request","rpcId":"r-7","method":"session/page","payload":{}}`,
			field: "payload.args", rpcID: "r-7",
		},
		{
			name:  "args 是数组",
			body:  `{"type":"client-request","rpcId":"r-8","method":"session/page","payload":{"args":[]}}`,
			field: "payload.args", rpcID: "r-8",
		},
		{
			name: "body 不是 JSON 对象",
			body: `[1,2,3]`, noReq: true,
		},
		{
			name: "body 是裸标量",
			body: `"nope"`, noReq: true,
		},
		{
			name: "body 是空",
			body: ``, noReq: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request, failure := ParseClientRequest("session/page", []byte(testCase.body))
			if failure == nil {
				t.Fatalf("这个形状必须 fail-loud，实际放过了：%s", testCase.body)
			}
			if failure.Code != CodeBadRequest {
				t.Fatalf("信封层面的拒绝必须是 %s，实际 %s", CodeBadRequest, failure.Code)
			}
			if testCase.noReq {
				if request != nil {
					t.Fatalf("非 JSON 对象应返回 nil 请求（调用方按 400 处理）")
				}
				return
			}
			if request == nil {
				t.Fatalf("非法但可解析的请求必须回填 rpcId，实际 nil")
			}
			if request.RPCID != testCase.rpcID {
				t.Fatalf("rpcId 应回填 %q，实际 %q", testCase.rpcID, request.RPCID)
			}
			gotField, _ := failure.Details["field"].(string)
			if gotField != testCase.field {
				t.Fatalf("details.field 应为 %q，实际 %q（%v）", testCase.field, gotField, failure.Details)
			}
		})
	}
}

// TestParseClientRequestEchoesWeirdRPCID 钉住"逐字回显"：rpcId 不是我们生成的，
// 不能规范化、不能 trim、不能重新编码成别的字节。
func TestParseClientRequestEchoesWeirdRPCID(t *testing.T) {
	weird := "a\"b\\c\n\té😀 " + strings.Repeat("x", 4096)
	body := envelopeBody(weird, "session/page", `{}`)
	request, failure := ParseClientRequest("session/page", []byte(body))
	if failure != nil {
		t.Fatalf("合法信封不该报错：%v", failure)
	}
	if request.RPCID != weird {
		t.Fatalf("rpcId 没有逐字保留：got=%q want=%q", request.RPCID, weird)
	}
	// 同时确认它在响应里也是同一串字节（不是被 JSON 转义两次的那种）。
	encoded, err := EncodeFailure(request.RPCID, Errorf(CodeInternal, "x"))
	if err != nil {
		t.Fatalf("编码失败响应: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("失败响应不是 JSON: %v", err)
	}
	if decoded["rpcId"] != weird {
		t.Fatalf("响应里的 rpcId 不是逐字回显")
	}
}

// TestDescriptorExactKeys 钉住 args 字段集校验（EVIDENCE §2.1：缺必填/多未知都拒）。
func TestDescriptorExactKeys(t *testing.T) {
	descriptor := &Descriptor{Required: []string{"path"}, Optional: []string{"options"}}

	ok := map[string]json.RawMessage{"path": json.RawMessage(`"a.txt"`), "options": json.RawMessage(`{}`)}
	if failure := descriptor.Validate("workspaceFiles/readBytes", ok); failure != nil {
		t.Fatalf("合法字段集不该报错：%v", failure)
	}

	extra := map[string]json.RawMessage{"path": json.RawMessage(`"a.txt"`), "bogus": json.RawMessage(`1`)}
	failure := descriptor.Validate("workspaceFiles/readBytes", extra)
	if failure == nil || failure.Code != CodeArgumentsInvalid {
		t.Fatalf("多一个字段必须 %s，实际 %v", CodeArgumentsInvalid, failure)
	}
	if failure.Details["field"] != "bogus" {
		t.Fatalf("details.field 应指向多出来的那个字段：%v", failure.Details)
	}
	if failure.Details["endpoint"] != "workspaceFiles/readBytes" {
		t.Fatalf("details.endpoint 缺失：%v", failure.Details)
	}

	missing := map[string]json.RawMessage{"options": json.RawMessage(`{}`)}
	failure = descriptor.Validate("workspaceFiles/readBytes", missing)
	if failure == nil || failure.Code != CodeArgumentsInvalid {
		t.Fatalf("缺必填字段必须 %s，实际 %v", CodeArgumentsInvalid, failure)
	}
	if failure.Details["field"] != "path" {
		t.Fatalf("details.field 应指向缺的字段：%v", failure.Details)
	}
}

// TestValidateArgsNilDescriptor 钉住"没登记 descriptor 就只允许空 args"。
//
// 这条是 fail-loud 的兜底：descriptor 忘登记时，带参数的方法会被拒，
// 而不是静默接受任意字段——后者会让"没有校验"看起来像"什么参数都对"。
func TestValidateArgsNilDescriptor(t *testing.T) {
	if failure := ValidateArgs(nil, "session/whatever", map[string]json.RawMessage{}); failure != nil {
		t.Fatalf("空 args 应放行：%v", failure)
	}
	failure := ValidateArgs(nil, "session/whatever", map[string]json.RawMessage{"x": json.RawMessage(`1`)})
	if failure == nil || failure.Code != CodeArgumentsInvalid {
		t.Fatalf("没登记 descriptor 却带了参数必须拒：%v", failure)
	}
}
