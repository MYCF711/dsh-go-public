package officialwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strconv"
)

// attachmentPartName 是 multipart 里 metadata 那个 part 的名字（EVIDENCE §5.1）。
const attachmentPartName = "metadata"

// codecBytes 是目前唯一允许的 codec（EVIDENCE §5.1 「codec 目前只允许 bytes」）。
const codecBytes = "bytes"

// Attachment 是 metadata JSON 里的附件声明：`{path, codec:"bytes", part:"bytes-0"}`。
//
// Path 是**键路径**：从 `result.value` 起逐段定位（对象用键名、数组用下标字符串），
// 被抽走的位置在 JSON 里留 `null` 占位（EVIDENCE §5.1）。
type Attachment struct {
	Path  []string `json:"path"`
	Codec string   `json:"codec"`
	Part  string   `json:"part"`
}

// EncodeResult 把结果值编成官方响应字节。
//
// 没有二进制时是普通 application/json；有二进制时是 multipart/form-data：
// metadata part 放**整个 server-response 信封**（多一个顶层 attachments 数组），
// 另外 N 个 `bytes-<i>` part 放裸字节（EVIDENCE §5.1）。
//
// 注意：`attachments` 放在信封顶层还是 result 里，取证只说到"metadata = 原 JSON
// 多出 attachments"（EVIDENCE §5.1），未逐字定位。本实现取**信封顶层**，
// 并在交付报告的"未验项"里登记——这是 M1 里唯一一处需要真机抓包确认的字节位置。
func EncodeResult(rpcID string, value any) (contentType string, body []byte, err error) {
	clean, atts, parts := collectAttachments(value)
	envelope := responseEnvelope{
		Type:   responseType,
		RPCID:  rpcID,
		Result: resultBody{OK: true, Value: clean},
	}
	if len(atts) == 0 {
		encoded, marshalErr := json.Marshal(envelope)
		if marshalErr != nil {
			return "", nil, marshalErr
		}
		return "application/json; charset=utf-8", encoded, nil
	}
	envelope.Attachments = atts
	meta, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		return "", nil, marshalErr
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Disposition", `form-data; name="`+attachmentPartName+`"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, createErr := writer.CreatePart(metaHeader)
	if createErr != nil {
		return "", nil, createErr
	}
	if _, writeErr := metaPart.Write(meta); writeErr != nil {
		return "", nil, writeErr
	}
	for i, part := range parts {
		header := textproto.MIMEHeader{}
		// filename 不是装饰：WHATWG `Response.formData()` 只有带 filename 的 part
		// 才会解析成 Blob，否则是 string，官方客户端 `client.node.js:366` 会抛
		// "invalid binary response fields"（L2 E2E P0-1 实测）。
		header.Set("Content-Disposition", `form-data; name="`+atts[i].Part+`"; filename="`+atts[i].Part+`"`)
		header.Set("Content-Type", "application/octet-stream")
		partWriter, partErr := writer.CreatePart(header)
		if partErr != nil {
			return "", nil, partErr
		}
		if _, writeErr := partWriter.Write(part); writeErr != nil {
			return "", nil, writeErr
		}
	}
	if closeErr := writer.Close(); closeErr != nil {
		return "", nil, closeErr
	}
	return writer.FormDataContentType(), buf.Bytes(), nil
}

// EncodeFailure 编一个业务失败响应（HTTP 仍然 200）。
func EncodeFailure(rpcID string, failure *Error) ([]byte, error) {
	return json.Marshal(NewFailure(rpcID, failure))
}

// collectAttachments 递归把 []byte 抽成附件。
//
// 只走 `map[string]any` / `[]any`：这两种正是 JSON 解码后的容器类型，
// 也是官方 encodeRuntimeResult 走的两种（EVIDENCE §5.1）。其它类型原样保留——
// 把未知结构"猜"成容器只会生成一条客户端定位不到的 path。
func collectAttachments(value any) (clean any, atts []Attachment, parts [][]byte) {
	var attachments []Attachment
	var bodies [][]byte
	clean = collectInto(value, nil, &attachments, &bodies)
	return clean, attachments, bodies
}

func collectInto(value any, path []string, atts *[]Attachment, parts *[][]byte) any {
	switch typed := value.(type) {
	case []byte:
		part := "bytes-" + strconv.Itoa(len(*atts))
		*atts = append(*atts, Attachment{
			Path:  append([]string(nil), path...),
			Codec: codecBytes,
			Part:  part,
		})
		*parts = append(*parts, typed)
		return nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = collectInto(item, childPath(path, key), atts, parts)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = collectInto(item, childPath(path, strconv.Itoa(i)), atts, parts)
		}
		return out
	default:
		return value
	}
}

func childPath(path []string, key string) []string {
	out := make([]string, len(path)+1)
	copy(out, path)
	out[len(path)] = key
	return out
}

// DecodeMultipartResponse 是客户端侧的官方解码器：multipart → 信封 + 字节回填。
//
// 它存在的意义是**闭环自证**：宿主编出来的字节，必须能被一段"照官方客户端规则写"
// 的代码解回原值（码表 codec、path 逐段定位、占位符必须是 null）。没有这一步，
// "我实现了 multipart" 就只是"我写了段序列化代码"。
//
// 返回的信封里 `attachments` 已被摘掉（官方客户端也是把它消费掉，不外传）。
func DecodeMultipartResponse(contentType string, body []byte) (map[string]any, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("官方 multipart 的 content-type 解析失败：%w", err)
	}
	if mediaType != "multipart/form-data" {
		return nil, fmt.Errorf("官方多段响应必须是 multipart/form-data，实际 %q", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("官方 multipart 缺 boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var meta []byte
	parts := map[string][]byte{}
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("读 multipart part 失败：%w", nextErr)
		}
		data, readErr := io.ReadAll(part)
		if readErr != nil {
			return nil, fmt.Errorf("读 multipart part 正文失败：%w", readErr)
		}
		if part.FormName() == attachmentPartName {
			meta = data
			continue
		}
		parts[part.FormName()] = data
	}
	if len(meta) == 0 {
		return nil, errors.New("官方 multipart 缺 metadata part")
	}
	var envelope map[string]any
	if err := json.Unmarshal(meta, &envelope); err != nil {
		return nil, fmt.Errorf("metadata 不是 JSON 对象：%w", err)
	}
	rawAtts, ok := envelope["attachments"]
	if !ok {
		return envelope, nil
	}
	list, ok := rawAtts.([]any)
	if !ok {
		return nil, errors.New("metadata.attachments 不是数组")
	}
	result, _ := envelope["result"].(map[string]any)
	if result == nil {
		return nil, errors.New("metadata 缺 result")
	}
	value := result["value"]
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("attachments 的条目不是对象")
		}
		codec, _ := entry["codec"].(string)
		if codec != codecBytes {
			return nil, fmt.Errorf("不支持的附件 codec：%q（官方当前只有 bytes）", codec)
		}
		partName, _ := entry["part"].(string)
		data, has := parts[partName]
		if !has {
			return nil, fmt.Errorf("metadata 声明了 part %q，但 multipart 里没有它", partName)
		}
		path, err := attachmentPath(entry["path"])
		if err != nil {
			return nil, err
		}
		if err := assignAtPath(value, path, data); err != nil {
			return nil, err
		}
	}
	delete(envelope, "attachments")
	return envelope, nil
}

// attachmentPath 把 JSON 里的 path 数组解成字符串切片。
func attachmentPath(raw any) ([]string, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("attachments 的 path 必须是非空数组")
	}
	path := make([]string, len(list))
	for i, segment := range list {
		text, ok := segment.(string)
		if !ok {
			return nil, errors.New("attachments 的 path 段必须是字符串")
		}
		path[i] = text
	}
	return path, nil
}

// assignAtPath 按 path 定位并写回字节，写之前校验占位符是 null。
//
// "占位符必须是 null" 是官方客户端的判据（EVIDENCE §5.1）：如果宿主忘了清空
// 原位置，客户端就无法判断这份字节是不是附件——与其猜，不如报错。
func assignAtPath(root any, path []string, data []byte) error {
	if len(path) == 0 {
		return errors.New("attachments 的 path 为空")
	}
	current := root
	for _, segment := range path[:len(path)-1] {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[segment]
			if !ok {
				return fmt.Errorf("attachments 的 path 找不到键 %q", segment)
			}
			current = next
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(node) {
				return fmt.Errorf("attachments 的 path 下标非法：%q", segment)
			}
			current = node[index]
		default:
			return fmt.Errorf("attachments 的 path 在 %q 处遇到了非容器", segment)
		}
	}
	last := path[len(path)-1]
	switch node := current.(type) {
	case map[string]any:
		existing, ok := node[last]
		if !ok {
			return fmt.Errorf("attachments 的 path 找不到键 %q", last)
		}
		if existing != nil {
			return fmt.Errorf("附件位置 %q 的占位符不是 null（宿主没清空原值）", last)
		}
		node[last] = data
		return nil
	case []any:
		index, err := strconv.Atoi(last)
		if err != nil || index < 0 || index >= len(node) {
			return fmt.Errorf("attachments 的 path 下标非法：%q", last)
		}
		if node[index] != nil {
			return fmt.Errorf("附件位置 [%d] 的占位符不是 null（宿主没清空原值）", index)
		}
		node[index] = data
		return nil
	default:
		return fmt.Errorf("attachments 的 path 末端不是容器（%q）", last)
	}
}
