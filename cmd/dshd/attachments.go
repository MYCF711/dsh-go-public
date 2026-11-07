package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

const (
	attachmentMaxBytes = 8 << 20
	// 图片解码出来的像素上限。只按字节数设限挡不住解压炸弹：几十字节的
	// JPEG/PNG 头就能声明 60000×60000，标准库会按声明尺寸先分配位图内存。
	// 8 MiB 的图片按 64M 像素折算已是 8 字节/像素，正常截图/照片远小于它。
	attachmentMaxPixels = 64 << 20
	promptMaxBlocks     = 64
	promptMaxTextBytes  = 1 << 20
)

var (
	attachmentIDPattern = regexp.MustCompile(`^att-[0-9a-f]{32}$`)
	pngSignature        = []byte("\x89PNG\r\n\x1a\n")
)

type attachmentStoreError struct {
	code    string
	status  int
	message string
	err     error
}

func (e *attachmentStoreError) Error() string {
	if e.err != nil {
		return e.message + ": " + e.err.Error()
	}
	return e.message
}

func (e *attachmentStoreError) Unwrap() error { return e.err }
func (e *attachmentStoreError) AttachmentError() (string, int) {
	return e.code, e.status
}

func attachmentError(code string, status int, message string, err error) error {
	return &attachmentStoreError{code: code, status: status, message: message, err: err}
}

func attachmentBadRequest(message string, err error) error {
	return attachmentError("bad_request", http.StatusBadRequest, message, err)
}

func attachmentNotFound(message string, err error) error {
	return attachmentError("not_found", http.StatusNotFound, message, err)
}

func attachmentInternal(message string, err error) error {
	return attachmentError("internal", http.StatusInternalServerError, message, err)
}

// attachmentStore 的唯一根在当前 home 下；不同 home 没有共享索引或全局对象。
type attachmentStore struct {
	home string
	root string
}

func newAttachmentStore(home string) *attachmentStore {
	return &attachmentStore{home: home, root: filepath.Join(home, "state", "attachments")}
}

func randomAttachmentID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", attachmentInternal("生成随机附件/引用 id 失败", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func cleanAuthorizedPath(path string) (string, os.FileInfo, error) {
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return "", nil, attachmentBadRequest("本地路径为空或含 NUL", nil)
	}
	if !filepath.IsAbs(path) {
		return "", nil, attachmentBadRequest("本地路径必须是用户显式给出的绝对路径", nil)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", nil, attachmentBadRequest("本地路径解析失败", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, attachmentNotFound("本地文件不存在", err)
		}
		return "", nil, attachmentBadRequest("本地路径不可读取", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, attachmentBadRequest("不接受符号链接；请显式给出真实常规文件路径", nil)
	}
	if !info.Mode().IsRegular() {
		return "", nil, attachmentBadRequest("本地路径不是常规文件", nil)
	}
	return abs, info, nil
}

func (s *attachmentStore) Put(path string) (transport.Attachment, error) {
	abs, info, err := cleanAuthorizedPath(path)
	if err != nil {
		return transport.Attachment{}, err
	}
	if info.Size() <= 0 {
		return transport.Attachment{}, attachmentBadRequest("附件不能是 0 bytes", nil)
	}
	if info.Size() > attachmentMaxBytes {
		return transport.Attachment{}, attachmentBadRequest(fmt.Sprintf("附件 %d bytes 超过 %d bytes 上限", info.Size(), attachmentMaxBytes), nil)
	}
	file, err := os.Open(abs)
	if err != nil {
		return transport.Attachment{}, attachmentBadRequest("打开授权文件失败", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, attachmentMaxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return transport.Attachment{}, attachmentBadRequest("读取授权文件失败", readErr)
	}
	if closeErr != nil {
		return transport.Attachment{}, attachmentBadRequest("关闭授权文件失败", closeErr)
	}
	if len(data) == 0 || len(data) > attachmentMaxBytes {
		return transport.Attachment{}, attachmentBadRequest("附件为空或超过大小上限", nil)
	}
	if int64(len(data)) != info.Size() {
		return transport.Attachment{}, attachmentBadRequest("读取期间文件大小发生变化，请重试", nil)
	}
	mime, width, height, err := inspectAttachment(data)
	if err != nil {
		return transport.Attachment{}, err
	}
	digest := sha256.Sum256(data)
	attachment := transport.Attachment{
		Name: filepath.Base(abs), ByteSize: int64(len(data)), MIME: mime,
		SHA256: hex.EncodeToString(digest[:]), Width: width, Height: height,
	}
	if err := os.MkdirAll(filepath.Join(s.root, "objects"), 0o700); err != nil {
		return transport.Attachment{}, attachmentInternal("创建附件对象目录失败", err)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "tmp"), 0o700); err != nil {
		return transport.Attachment{}, attachmentInternal("创建附件临时目录失败", err)
	}
	for attempts := 0; attempts < 4; attempts++ {
		attachment.ID, err = randomAttachmentID("att-")
		if err != nil {
			return transport.Attachment{}, err
		}
		staging, err := os.MkdirTemp(filepath.Join(s.root, "tmp"), "put-")
		if err != nil {
			return transport.Attachment{}, attachmentInternal("创建附件 staging 失败", err)
		}
		metadata, err := json.Marshal(attachment)
		if err == nil {
			err = os.WriteFile(filepath.Join(staging, "blob"), data, 0o600)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(staging, "meta.json"), metadata, 0o600)
		}
		if err != nil {
			_ = os.RemoveAll(staging)
			return transport.Attachment{}, attachmentInternal("写入受控附件 store 失败", err)
		}
		destination := filepath.Join(s.root, "objects", attachment.ID)
		if err := os.Rename(staging, destination); err != nil {
			_ = os.RemoveAll(staging)
			if os.IsExist(err) {
				continue
			}
			return transport.Attachment{}, attachmentInternal("提交附件对象失败", err)
		}
		return attachment, nil
	}
	return transport.Attachment{}, attachmentInternal("随机附件 id 多次冲突", nil)
}

// inspectAttachment 只信字节：按内容嗅探 MIME，图片还必须**完整解码**通过。
//
// 只认 signature 或 image.DecodeConfig 是不够的：33 字节的"只有 signature + IHDR、
// 没有 IDAT/IEND"的坏 PNG 也能让 DecodeConfig 报出 3×2，Put 于是返回 200 并落了盘
// （W83 独立探针实测）。所以这里分两层：
//
//	① 容器结构：PNG 走完 chunk 表（CRC、IHDR 打头、至少一个 IDAT、IEND 收尾），
//	   GIF 校验头与结尾 trailer，WebP 走完整个 RIFF chunk 表并解出位流头尺寸；
//	② 让标准库把整幅图解码出来（PNG/JPEG/GIF），截断、CRC 错、像素数据不足都会报错。
//
// 任何一层失败都必须在此之前返回：Put 在调用本函数之前不会建 objects/tmp 目录，
// 所以坏图不会留下对象或 staging。
func inspectAttachment(data []byte) (mime string, width, height int, err error) {
	mime = http.DetectContentType(data)
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		mime = "image/webp"
	}
	switch mime {
	case "image/png":
		if structureErr := validatePNGStructure(data); structureErr != nil {
			return "", 0, 0, attachmentBadRequest("PNG 结构不完整或已损坏", structureErr)
		}
		return decodeWholeImage(data, "png", mime)
	case "image/jpeg":
		return decodeWholeImage(data, "jpeg", mime)
	case "image/gif":
		if structureErr := validateGIFStructure(data); structureErr != nil {
			return "", 0, 0, attachmentBadRequest("GIF 结构不完整或已损坏", structureErr)
		}
		return decodeWholeImage(data, "gif", mime)
	case "image/webp":
		width, height, decodeErr := webpDimensions(data)
		if decodeErr != nil {
			return "", 0, 0, attachmentBadRequest("WebP 结构不完整或已损坏", decodeErr)
		}
		if budgetErr := checkImagePixelBudget(width, height); budgetErr != nil {
			return "", 0, 0, budgetErr
		}
		return mime, width, height, nil
	}
	if utf8.Valid(data) && !bytes.ContainsRune(data, '\x00') {
		return "text/plain", 0, 0, nil
	}
	return "", 0, 0, attachmentBadRequest("仅支持 PNG/JPEG/GIF/WebP 图片或 UTF-8 文本附件；检测到 "+mime, nil)
}

// decodeWholeImage 先只读头拿尺寸（DecodeConfig 不分配像素内存），守住解码上限，
// 再让标准库完整解码整幅图；返回的仍是头部声明的尺寸（GIF 的逻辑屏幕尺寸在
// DecodeConfig 里，首帧 rect 可能更小，元数据语义保持与之前一致）。
func decodeWholeImage(data []byte, wantFormat, mime string) (string, int, int, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, attachmentBadRequest("图片头无法解析", err)
	}
	if format != wantFormat {
		return "", 0, 0, attachmentBadRequest("图片编码与嗅探出的 MIME 不一致："+format, nil)
	}
	if err := checkImagePixelBudget(config.Width, config.Height); err != nil {
		return "", 0, 0, err
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", 0, 0, attachmentBadRequest("图片数据不完整，无法完整解码", err)
	}
	if decoded == nil || decodedFormat != wantFormat {
		return "", 0, 0, attachmentBadRequest("图片编码与嗅探出的 MIME 不一致", nil)
	}
	if bounds := decoded.Bounds(); bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return "", 0, 0, attachmentBadRequest("图片解码后的尺寸无效", nil)
	}
	return mime, config.Width, config.Height, nil
}

func checkImagePixelBudget(width, height int) error {
	if width <= 0 || height <= 0 {
		return attachmentBadRequest("图片尺寸无效", nil)
	}
	if pixels := int64(width) * int64(height); pixels > attachmentMaxPixels {
		return attachmentBadRequest(fmt.Sprintf("图片 %d×%d 共 %d 像素，超过 %d 像素解码上限", width, height, pixels, attachmentMaxPixels), nil)
	}
	return nil
}

// validatePNGStructure 走完整张 PNG 的 chunk 表：CRC 必须对得上（标准库也会校验），
// 第一个 chunk 必须是 13 字节 IHDR，IEND 必须是零长度且之前至少有一个 IDAT。
// 只有 signature + IHDR 的截断文件会在这里因为"读到文件末尾也没有 IEND"被拒。
func validatePNGStructure(data []byte) error {
	if !bytes.HasPrefix(data, pngSignature) {
		return errors.New("PNG signature 不符")
	}
	offset := len(pngSignature)
	seenIHDR := false
	idatChunks := 0
	for {
		if offset+8 > len(data) {
			return errors.New("PNG 在 IEND 之前就结束了")
		}
		length := int64(binary.BigEndian.Uint32(data[offset : offset+4]))
		kind := string(data[offset+4 : offset+8])
		for i := 0; i < 4; i++ {
			c := data[offset+4+i]
			if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
				return fmt.Errorf("PNG chunk 类型 %q 不是 ASCII 字母", kind)
			}
		}
		if length > int64(len(data)-offset-12) {
			return fmt.Errorf("PNG %s chunk 长度 %d 越界", kind, length)
		}
		end := offset + 12 + int(length)
		if want := binary.BigEndian.Uint32(data[end-4 : end]); crc32.ChecksumIEEE(data[offset+4:end-4]) != want {
			return fmt.Errorf("PNG %s chunk CRC 不符", kind)
		}
		if !seenIHDR {
			if kind != "IHDR" || length != 13 {
				return errors.New("PNG 第一个 chunk 不是 13 字节 IHDR")
			}
			seenIHDR = true
		}
		switch kind {
		case "IDAT":
			idatChunks++
		case "IEND":
			if length != 0 {
				return errors.New("PNG IEND 长度不为 0")
			}
			if idatChunks == 0 {
				return errors.New("PNG 没有 IDAT 图像数据")
			}
			return nil
		}
		offset = end
	}
}

// validateGIFStructure 补标准库的缺口：gif.Decode 在文件正好截掉结尾 trailer
// （0x3B）时仍会成功，但那已经是截断文件。头 + 逻辑屏幕描述符至少 14 字节。
func validateGIFStructure(data []byte) error {
	if len(data) < 14 {
		return errors.New("GIF 太短")
	}
	if head := string(data[:6]); head != "GIF87a" && head != "GIF89a" {
		return errors.New("GIF 头不是 GIF87a/GIF89a")
	}
	if data[len(data)-1] != 0x3b {
		return errors.New("GIF 缺少结尾 trailer（0x3B），文件被截断")
	}
	return nil
}

// webpDimensions 走完整个 RIFF 容器：声明长度必须等于实际字节数，每个 chunk
// 不得越界，并且至少有一个真正的图像数据 chunk（VP8/VP8L/ANMF）能按位流头
// 解出尺寸；VP8X 声明画布尺寸时以画布为准。标准库没有 WebP 解码器，
// 这里做结构校验而不是完整解码——但不再只看第一个 chunk 的头。
func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 20 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, errors.New("不是完整 RIFF WebP")
	}
	declared := int64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if declared != int64(len(data)) {
		return 0, 0, fmt.Errorf("RIFF 声明长度 %d 与实际 %d 不符", declared, len(data))
	}
	canvasWidth, canvasHeight := 0, 0
	simpleWidth, simpleHeight := 0, 0
	bitstreams := 0
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return 0, 0, errors.New("WebP chunk 头越界")
		}
		kind := string(data[offset : offset+4])
		length := int64(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if length > int64(len(data)-offset-8) {
			return 0, 0, fmt.Errorf("WebP %s chunk 长度 %d 越界", kind, length)
		}
		payload := data[offset+8 : offset+8+int(length)]
		offset += 8 + int(length) + int(length&1) // RIFF chunk 按偶数字节对齐
		switch kind {
		case "VP8X":
			if len(payload) < 10 {
				return 0, 0, errors.New("VP8X chunk 太短")
			}
			canvasWidth = 1 + int(payload[4]) + int(payload[5])<<8 + int(payload[6])<<16
			canvasHeight = 1 + int(payload[7]) + int(payload[8])<<8 + int(payload[9])<<16
		case "VP8 ":
			width, height, err := webpVP8Dimensions(payload)
			if err != nil {
				return 0, 0, err
			}
			if simpleWidth == 0 {
				simpleWidth, simpleHeight = width, height
			}
			bitstreams++
		case "VP8L":
			width, height, err := webpVP8LDimensions(payload)
			if err != nil {
				return 0, 0, err
			}
			if simpleWidth == 0 {
				simpleWidth, simpleHeight = width, height
			}
			bitstreams++
		case "ANMF":
			if len(payload) < 24 {
				return 0, 0, errors.New("ANMF chunk 太短")
			}
			x := int(payload[0]) | int(payload[1])<<8 | int(payload[2])<<16
			y := int(payload[3]) | int(payload[4])<<8 | int(payload[5])<<16
			frameWidth := 1 + int(payload[6]) + int(payload[7])<<8 + int(payload[8])<<16
			frameHeight := 1 + int(payload[9]) + int(payload[10])<<8 + int(payload[11])<<16
			subKind := string(payload[16:20])
			subLength := int64(binary.LittleEndian.Uint32(payload[20:24]))
			if subLength > int64(len(payload)-24) {
				return 0, 0, errors.New("ANMF 内部图像 chunk 长度越界")
			}
			subPayload := payload[24 : 24+int(subLength)]
			var width, height int
			var frameErr error
			switch subKind {
			case "VP8 ":
				width, height, frameErr = webpVP8Dimensions(subPayload)
			case "VP8L":
				width, height, frameErr = webpVP8LDimensions(subPayload)
			default:
				return 0, 0, fmt.Errorf("ANMF 内部 chunk %q 不是 VP8/VP8L", subKind)
			}
			if frameErr != nil {
				return 0, 0, frameErr
			}
			if width != frameWidth || height != frameHeight {
				return 0, 0, fmt.Errorf("ANMF 声明 %d×%d 与内部位流 %d×%d 不符", frameWidth, frameHeight, width, height)
			}
			if canvasWidth > 0 && (x+frameWidth > canvasWidth || y+frameHeight > canvasHeight) {
				return 0, 0, errors.New("ANMF 帧矩形超出 VP8X 画布")
			}
			bitstreams++
		case "ANIM":
			if len(payload) != 6 {
				return 0, 0, errors.New("ANIM chunk 长度必须为 6")
			}
		case "ALPH":
			if len(payload) == 0 {
				return 0, 0, errors.New("ALPH chunk 为空")
			}
		case "ICCP", "EXIF", "XMP ":
			// 元数据 chunk：不影响图片有效性。
		default:
			// 未知 chunk 按 RIFF 语义跳过（前向兼容）；图片有效性由上面的
			// VP8/VP8L/ANMF 位流头保证，没有它们的文件在循环结束后被拒。
		}
	}
	if bitstreams == 0 {
		return 0, 0, errors.New("WebP 没有 VP8/VP8L/ANMF 图像数据 chunk")
	}
	if canvasWidth > 0 {
		return canvasWidth, canvasHeight, nil
	}
	if simpleWidth <= 0 || simpleHeight <= 0 {
		return 0, 0, errors.New("WebP 没有可用的图像尺寸")
	}
	return simpleWidth, simpleHeight, nil
}

func webpVP8Dimensions(payload []byte) (int, int, error) {
	if len(payload) < 10 {
		return 0, 0, errors.New("VP8 位流头太短")
	}
	if payload[0]&0x01 != 0 {
		return 0, 0, errors.New("VP8 首个位流不是关键帧")
	}
	if !bytes.Equal(payload[3:6], []byte{0x9d, 0x01, 0x2a}) {
		return 0, 0, errors.New("VP8 帧头起始码错误")
	}
	width := int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff)
	height := int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff)
	if width <= 0 || height <= 0 {
		return 0, 0, errors.New("VP8 尺寸为零")
	}
	return width, height, nil
}

func webpVP8LDimensions(payload []byte) (int, int, error) {
	if len(payload) < 5 || payload[0] != 0x2f {
		return 0, 0, errors.New("VP8L 签名错误")
	}
	bits := uint32(payload[1]) | uint32(payload[2])<<8 | uint32(payload[3])<<16 | uint32(payload[4])<<24
	if version := (bits >> 29) & 0x07; version != 0 {
		return 0, 0, fmt.Errorf("VP8L 版本 %d 不受支持", version)
	}
	width := 1 + int(bits&0x3fff)
	height := 1 + int((bits>>14)&0x3fff)
	return width, height, nil
}

func (s *attachmentStore) Get(id string) (transport.AttachmentContent, error) {
	if !attachmentIDPattern.MatchString(id) {
		return transport.AttachmentContent{}, attachmentBadRequest("attachment id 形状非法", nil)
	}
	dir := filepath.Join(s.root, "objects", id)
	metadata, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return transport.AttachmentContent{}, attachmentNotFound("附件不存在于当前 home", err)
		}
		return transport.AttachmentContent{}, attachmentInternal("读取附件元数据失败", err)
	}
	var attachment transport.Attachment
	if err := json.Unmarshal(metadata, &attachment); err != nil || attachment.ID != id {
		return transport.AttachmentContent{}, attachmentInternal("附件元数据损坏或 id 不符", err)
	}
	file, err := os.Open(filepath.Join(dir, "blob"))
	if err != nil {
		return transport.AttachmentContent{}, attachmentInternal("读取附件字节失败", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, attachmentMaxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return transport.AttachmentContent{}, attachmentInternal("读取附件字节失败", readErr)
	}
	if closeErr != nil {
		return transport.AttachmentContent{}, attachmentInternal("关闭附件字节失败", closeErr)
	}
	if len(data) == 0 || len(data) > attachmentMaxBytes {
		return transport.AttachmentContent{}, attachmentInternal("附件字节为空或超过大小上限", nil)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != attachment.ByteSize || hex.EncodeToString(digest[:]) != attachment.SHA256 {
		return transport.AttachmentContent{}, attachmentInternal("附件字节与持久 SHA256/byteSize 不符", nil)
	}
	mime, width, height, inspectErr := inspectAttachment(data)
	if inspectErr != nil {
		return transport.AttachmentContent{}, attachmentInternal("附件真实 MIME/尺寸校验失败", inspectErr)
	}
	if attachment.MIME != mime || attachment.Width != width || attachment.Height != height {
		return transport.AttachmentContent{}, attachmentInternal("附件元数据 MIME/尺寸与真实字节不符", nil)
	}
	return transport.AttachmentContent{Attachment: attachment, Data: data}, nil
}

func (s *attachmentStore) ResolveBlocks(input []transport.ContentBlock) ([]transport.ContentBlock, error) {
	if len(input) == 0 || len(input) > promptMaxBlocks {
		return nil, attachmentBadRequest(fmt.Sprintf("content 块数量必须为 1..%d", promptMaxBlocks), nil)
	}
	out := make([]transport.ContentBlock, len(input))
	meaningful := false
	textBytes := 0
	for i, block := range input {
		switch block.Type {
		case "text":
			textBytes += len(block.Text)
			if textBytes > promptMaxTextBytes {
				return nil, attachmentBadRequest("文本块总大小超过 1 MiB", nil)
			}
			out[i] = transport.ContentBlock{Type: "text", Text: block.Text}
			meaningful = meaningful || strings.TrimSpace(block.Text) != ""
		case "attachment":
			content, err := s.Get(block.AttachmentID)
			if err != nil {
				return nil, err
			}
			a := content.Attachment
			out[i] = transport.ContentBlock{Type: "attachment", AttachmentID: a.ID, Name: a.Name,
				ByteSize: a.ByteSize, MIME: a.MIME, SHA256: a.SHA256, Width: a.Width, Height: a.Height}
			meaningful = true
		case "reference":
			id, err := randomAttachmentID("ref-")
			if err != nil {
				return nil, err
			}
			switch block.ReferenceType {
			case "file":
				abs, info, err := cleanAuthorizedPath(block.Path)
				if err != nil {
					return nil, err
				}
				out[i] = transport.ContentBlock{Type: "reference", ReferenceID: id, ReferenceType: "file",
					Path: abs, Name: filepath.Base(abs), ByteSize: info.Size()}
			case "session":
				if block.SessionID == "" {
					return nil, attachmentBadRequest("session reference 缺 sessionId", nil)
				}
				if _, err := session.FindByID(s.home, block.SessionID); err != nil {
					return nil, attachmentNotFound("session reference 目标不存在于当前 home", err)
				}
				out[i] = transport.ContentBlock{Type: "reference", ReferenceID: id, ReferenceType: "session", SessionID: block.SessionID}
			default:
				return nil, attachmentBadRequest("referenceType 只支持 file 或 session", nil)
			}
			meaningful = true
		default:
			return nil, attachmentBadRequest("未知 content block type: "+block.Type, nil)
		}
	}
	if !meaningful {
		return nil, attachmentBadRequest("content 不能全是空文本", nil)
	}
	return out, nil
}

func sessionBlocksFromTransport(input []transport.ContentBlock) []session.ContentBlock {
	out := make([]session.ContentBlock, len(input))
	for i, b := range input {
		out[i] = session.ContentBlock{Type: b.Type, Text: b.Text, AttachmentID: b.AttachmentID,
			Name: b.Name, ByteSize: b.ByteSize, MIME: b.MIME, SHA256: b.SHA256,
			Width: b.Width, Height: b.Height, ReferenceID: b.ReferenceID,
			ReferenceType: b.ReferenceType, Path: b.Path, SessionID: b.SessionID}
	}
	return out
}

func transportBlocksFromSession(input []session.ContentBlock) []transport.ContentBlock {
	out := make([]transport.ContentBlock, len(input))
	for i, b := range input {
		out[i] = transport.ContentBlock{Type: b.Type, Text: b.Text, AttachmentID: b.AttachmentID,
			Name: b.Name, ByteSize: b.ByteSize, MIME: b.MIME, SHA256: b.SHA256,
			Width: b.Width, Height: b.Height, ReferenceID: b.ReferenceID,
			ReferenceType: b.ReferenceType, Path: b.Path, SessionID: b.SessionID}
	}
	return out
}
