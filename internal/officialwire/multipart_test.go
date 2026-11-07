package officialwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strings"
	"testing"
)

// TestEncodeResultPlainJSONWithoutBytes 钉住"没有二进制就不套 multipart"。
func TestEncodeResultPlainJSONWithoutBytes(t *testing.T) {
	contentType, body, err := EncodeResult("r-1", map[string]any{"cursor": 3.0, "records": []any{}})
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("没有二进制时必须是 application/json，实际 %q", contentType)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("正文不是 JSON：%v", err)
	}
	if _, has := envelope["attachments"]; has {
		t.Fatalf("没有二进制时不该出现 attachments 键：%s", body)
	}
	if envelope["rpcId"] != "r-1" {
		t.Fatalf("rpcId 缺失：%s", body)
	}
}

// TestMultipartAttachmentsRoundTrip 钉住附件编解码闭环（EVIDENCE §5.1）。
func TestMultipartAttachmentsRoundTrip(t *testing.T) {
	value := map[string]any{
		"offset": 0.0,
		"items": []any{
			map[string]any{"data": []byte("first")},
			map[string]any{"data": []byte("second")},
		},
		"blob": []byte("third"),
		"eof":  true,
	}
	contentType, body, err := EncodeResult("r-bytes", value)
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("有二进制必须 multipart，实际 %q", contentType)
	}

	// 1) metadata 里的 attachments 声明必须逐条对上键路径，原位置必须是 null。
	meta := readMetadataPart(t, contentType, body)
	rawAtts, ok := meta["attachments"].([]any)
	if !ok || len(rawAtts) != 3 {
		t.Fatalf("应有 3 个附件声明，实际 %v", meta["attachments"])
	}
	paths := map[string]bool{}
	for _, raw := range rawAtts {
		entry, _ := raw.(map[string]any)
		codec, _ := entry["codec"].(string)
		if codec != codecBytes {
			t.Fatalf("codec 必须是 bytes，实际 %q", codec)
		}
		part, _ := entry["part"].(string)
		if !strings.HasPrefix(part, "bytes-") {
			t.Fatalf("part 名必须是 bytes-<i>，实际 %q", part)
		}
		segments, _ := entry["path"].([]any)
		joined := make([]string, len(segments))
		for i, segment := range segments {
			joined[i] = segment.(string)
		}
		paths[strings.Join(joined, ".")] = true
	}
	for _, want := range []string{"items.0.data", "items.1.data", "blob"} {
		if !paths[want] {
			t.Fatalf("缺 path %q（实际 %v）", want, paths)
		}
	}
	result := meta["result"].(map[string]any)
	valueMeta := result["value"].(map[string]any)
	if valueMeta["blob"] != nil {
		t.Fatalf("被抽走的位置必须是 null 占位，实际 %v", valueMeta["blob"])
	}
	items := valueMeta["items"].([]any)
	if items[0].(map[string]any)["data"] != nil {
		t.Fatalf("数组里的占位符也必须是 null")
	}

	// 2) 客户端解码必须把字节按 path 放回去。
	envelope, err := DecodeMultipartResponse(contentType, body)
	if err != nil {
		t.Fatalf("DecodeMultipartResponse: %v", err)
	}
	if _, has := envelope["attachments"]; has {
		t.Fatalf("解码后 attachments 应被消费掉")
	}
	restored := envelope["result"].(map[string]any)["value"].(map[string]any)
	if string(restored["blob"].([]byte)) != "third" {
		t.Fatalf("blob 还原失败：%v", restored["blob"])
	}
	restoredItems := restored["items"].([]any)
	first := restoredItems[0].(map[string]any)["data"].([]byte)
	second := restoredItems[1].(map[string]any)["data"].([]byte)
	if string(first) != "first" || string(second) != "second" {
		t.Fatalf("数组元素还原失败：%q %q", first, second)
	}
	if restored["eof"] != true || restored["offset"] != 0.0 {
		t.Fatalf("非二进制字段必须原样保留：%v", restored)
	}
}

// TestDecodeRejectsNonNullPlaceholder 钉住"占位符必须是 null"的校验。
func TestDecodeRejectsNonNullPlaceholder(t *testing.T) {
	meta := map[string]any{
		"type":   responseType,
		"rpcId":  "r-x",
		"result": map[string]any{"ok": true, "value": map[string]any{"data": "not-null"}},
		"attachments": []any{
			map[string]any{"path": []any{"data"}, "codec": codecBytes, "part": "bytes-0"},
		},
	}
	contentType, body := buildMultipart(t, meta, map[string][]byte{"bytes-0": []byte("payload")})
	if _, err := DecodeMultipartResponse(contentType, body); err == nil {
		t.Fatalf("占位符不是 null 必须报错，不能猜")
	}
}

// TestDecodeRejectsUnknownCodec 钉住"只有 bytes 一种 codec"。
func TestDecodeRejectsUnknownCodec(t *testing.T) {
	meta := map[string]any{
		"type":   responseType,
		"rpcId":  "r-x",
		"result": map[string]any{"ok": true, "value": map[string]any{"data": nil}},
		"attachments": []any{
			map[string]any{"path": []any{"data"}, "codec": "utf8", "part": "bytes-0"},
		},
	}
	contentType, body := buildMultipart(t, meta, map[string][]byte{"bytes-0": []byte("payload")})
	if _, err := DecodeMultipartResponse(contentType, body); err == nil {
		t.Fatalf("未知 codec 必须报错")
	}
}

// TestEncodeFailureHasObjectDetails 钉住 error.details 永远是对象（不能是 null）。
func TestEncodeFailureHasObjectDetails(t *testing.T) {
	body, err := EncodeFailure("r-fail", NewError("session/not-found", "没了", nil))
	if err != nil {
		t.Fatalf("EncodeFailure: %v", err)
	}
	if !bytes.Contains(body, []byte(`"details":{}`)) {
		t.Fatalf("details 为 nil 时必须序列化成 {}，实际 %s", body)
	}
}

// readMetadataPart 把 multipart 的 metadata part 读出来。
func readMetadataPart(t *testing.T, contentType string, body []byte) map[string]any {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("解析 content-type: %v", err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatalf("读第一个 part: %v", err)
	}
	if part.FormName() != attachmentPartName {
		t.Fatalf("第一个 part 必须是 metadata，实际 %q", part.FormName())
	}
	var meta map[string]any
	if err := json.NewDecoder(part).Decode(&meta); err != nil {
		t.Fatalf("metadata 不是 JSON: %v", err)
	}
	return meta
}

// buildMultipart 手工造一份官方多段响应（用来测"客户端侧拒绝坏数据"）。
func buildMultipart(t *testing.T, meta map[string]any, parts map[string][]byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("序列化 metadata: %v", err)
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="`+attachmentPartName+`"`)
	header.Set("Content-Type", "application/json")
	metaPart, err := writer.CreatePart(header)
	if err != nil {
		t.Fatalf("CreatePart(metadata): %v", err)
	}
	if _, err := metaPart.Write(encoded); err != nil {
		t.Fatalf("写 metadata: %v", err)
	}
	for name, data := range parts {
		partHeader := textproto.MIMEHeader{}
		partHeader.Set("Content-Disposition", `form-data; name="`+name+`"`)
		part, err := writer.CreatePart(partHeader)
		if err != nil {
			t.Fatalf("CreatePart(%s): %v", name, err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("写 part %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭 multipart: %v", err)
	}
	return writer.FormDataContentType(), buf.Bytes()
}

// formDataEntry 是 WHATWG `formData().get(name)` 能返回的两种值的最小模型。
//
// 官方客户端只接受其中一种：`client.node.js:366` 的
//
//	if (!(data instanceof Blob)) throw new TypeError("connection: invalid binary response fields")
//
// 所以"part 的值是 Blob 还是 string"不是风格问题，是**能不能连上**的判据。
// 决定因素是 Content-Disposition 的 `filename` 参数在不在（HTML 标准
// §4.10.22.7「multipart/form-data」）：带 → File(Blob)，不带 → string。
type formDataEntry struct {
	kind        string // "Blob" 或 "string"
	blob        []byte // kind=Blob 时的原始字节
	text        string // kind=string 时按 UTF-8 解码出来的值
	fileName    string // Go 的 mime/multipart 视角：Part.FileName()
	disposition string // 原样 header（断言失败时贴出来）
}

// parseOfficialFormData 按 WHATWG `multipart/form-data` 的解析规则把下行响应
// 读成 `Response.formData()` 会拿到的值。
//
// 为什么不能只断言 header 字符串：缺 `filename` 时
// `form-data; name="bytes-0"` 是一条**完全合法**的 header，肉眼和字符串比较
// 都发现不了；只有经过这条解析规则，值才会从 Blob 退化成 string。
func parseOfficialFormData(t *testing.T, contentType string, body []byte) map[string]formDataEntry {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("解析 content-type: %v", err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("官方解析路径只处理 multipart/form-data，实际 %q", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("multipart 缺 boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	fields := map[string]formDataEntry{}
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("读 multipart part: %v", nextErr)
		}
		data, readErr := io.ReadAll(part)
		if readErr != nil {
			t.Fatalf("读 multipart part 正文: %v", readErr)
		}
		disposition := part.Header.Get("Content-Disposition")
		_, dispositionParams, parseErr := mime.ParseMediaType(disposition)
		if parseErr != nil {
			t.Fatalf("解析 Content-Disposition %q: %v", disposition, parseErr)
		}
		entry := formDataEntry{fileName: part.FileName(), disposition: disposition}
		if _, isFile := dispositionParams["filename"]; isFile {
			entry.kind = "Blob"
			entry.blob = data
		} else {
			entry.kind = "string"
			entry.text = string(data)
		}
		fields[part.FormName()] = entry
	}
	return fields
}

// TestEncodeResultBinaryPartIsBlobUnderFormData 是 L2 E2E P0-1 的回归测试。
//
// 修复前：二进制 part 的 Content-Disposition 只有 `name="bytes-0"`，
// WHATWG `Response.formData()` 把它解析成 **string**，官方客户端在
// `client.node.js:366` 抛 `connection: invalid binary response fields`，
// 整个 `workspaceFiles/readBytes` 不可用。
//
// 本用例断言三件事，缺一不可：
//  1. 判据本体：`formData().get("<part>")` 是 **Blob**（不是 string）；
//  2. 字节完全一致：用非 UTF-8 字节，string 路径必然把它改写成 U+FFFD；
//  3. 第二重独立证据：Go 的 `mime/multipart` 也看到 `Part.FileName() != ""`。
func TestEncodeResultBinaryPartIsBlobUnderFormData(t *testing.T) {
	// 故意含 0x00/0xFF/CRLF 与非法 UTF-8 序列：只要 part 退化成 string，
	// 解码出来的字节就不可能与原文逐字相等 —— 这条用例因此不空转。
	first := []byte{0x00, 0x01, 0xff, 0xfe, '\r', '\n', 'd', 's', 'h', 0x80}
	second := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, '\r', '\n'}
	value := map[string]any{
		"offset": 0.0,
		"data":   first,
		"extra":  []any{second},
		"eof":    true,
	}
	contentType, body, err := EncodeResult("r-blob", value)
	if err != nil {
		t.Fatalf("EncodeResult: %v", err)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("含二进制必须走 multipart/form-data，实际 %q", contentType)
	}

	// 先按 metadata 的声明把"part 名 → 期望字节"建出来：part 的编号取决于
	// collectAttachments 的遍历顺序（map 顺序不稳定），不能硬编码 bytes-0/1。
	meta := readMetadataPart(t, contentType, body)
	rawAtts, ok := meta["attachments"].([]any)
	if !ok || len(rawAtts) == 0 {
		t.Fatalf("metadata 必须带 attachments 声明：%v", meta["attachments"])
	}
	wantByPath := map[string][]byte{"data": first, "extra.0": second}
	declared := map[string][]byte{}
	for _, raw := range rawAtts {
		entry, _ := raw.(map[string]any)
		partName, _ := entry["part"].(string)
		segments, _ := entry["path"].([]any)
		joined := make([]string, len(segments))
		for i, segment := range segments {
			joined[i] = segment.(string)
		}
		path := strings.Join(joined, ".")
		want, expected := wantByPath[path]
		if !expected {
			t.Fatalf("附件 path %q 不在预期内（预期 %v）", path, wantByPath)
		}
		declared[partName] = want
	}
	if len(declared) != len(wantByPath) {
		t.Fatalf("应有 %d 个附件 part，实际 %v", len(wantByPath), meta["attachments"])
	}

	fields := parseOfficialFormData(t, contentType, body)
	for partName, want := range declared {
		entry, present := fields[partName]
		if !present {
			t.Fatalf("formData() 里没有 part %q（实际 part：%v）", partName, formDataNames(fields))
		}
		// 判据 1：官方客户端拿到的必须是 Blob，不能是 string。
		if entry.kind != "Blob" {
			t.Fatalf("formData().get(%q) 是 %s 而不是 Blob —— 官方 client.node.js:366 会抛 "+
				"`connection: invalid binary response fields`；Content-Disposition=%q",
				partName, entry.kind, entry.disposition)
		}
		// 判据 2：字节逐字一致（string 路径会把非法 UTF-8 改写成 U+FFFD）。
		if !bytes.Equal(entry.blob, want) {
			t.Fatalf("part %q 字节不一致：got=%v want=%v", partName, entry.blob, want)
		}
		// 判据 3（第二重独立证据）：Go 的 mime/multipart 也必须看到 filename 参数。
		if entry.fileName == "" {
			t.Fatalf("part %q 的 Part.FileName() 为空：Content-Disposition 没带 filename 参数（%q）",
				partName, entry.disposition)
		}
	}
}

// TestOfficialFormDataMissingFilenameIsStringNotBlob 是上一条用例的反证（不空转）：
// 手工造一份**没有 filename** 的 multipart —— 正是修复前 EncodeResult 的字节形状——
// 同一条解析路径必须把它判成 string。它证明上一条断言真的能抓住缺失的 filename，
// 而不是"怎么都能过"。
func TestOfficialFormDataMissingFilenameIsStringNotBlob(t *testing.T) {
	meta := map[string]any{
		"type":   responseType,
		"rpcId":  "r-no-filename",
		"result": map[string]any{"ok": true, "value": map[string]any{"data": nil}},
		"attachments": []any{
			map[string]any{"path": []any{"data"}, "codec": codecBytes, "part": "bytes-0"},
		},
	}
	contentType, body := buildMultipart(t, meta, map[string][]byte{"bytes-0": []byte("hello")})
	fields := parseOfficialFormData(t, contentType, body)
	entry, present := fields["bytes-0"]
	if !present {
		t.Fatalf("没有 part bytes-0（实际 %v）", formDataNames(fields))
	}
	if entry.kind != "string" {
		t.Fatalf("没有 filename 的 part 必须按 WHATWG 判成 string，实际 %s（%q）",
			entry.kind, entry.disposition)
	}
	if entry.fileName != "" {
		t.Fatalf("Part.FileName() 应为空，实际 %q", entry.fileName)
	}
}

// formDataNames 把 part 名排序后返回，只用于失败信息。
func formDataNames(fields map[string]formDataEntry) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
