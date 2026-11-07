package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dshgo/internal/llm"
	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// writePromptContentFile 把父进程已解析的块交给同一 dshd 的 run 子进程。
// 文件只含元数据，附件字节仍只从当前 home 的受控 store 读取。
func writePromptContentFile(home string, blocks []transport.ContentBlock) (string, error) {
	dir := filepath.Join(home, "state", "prompt-blocks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", attachmentInternal("创建 prompt block 交接目录失败", err)
	}
	id, err := randomAttachmentID("prompt-")
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(blocks)
	if err != nil {
		return "", attachmentInternal("序列化 prompt content blocks 失败", err)
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", attachmentInternal("写入 prompt content blocks 交接文件失败", err)
	}
	return path, nil
}

// loadPromptContentForRun 同时给出 durable 块与模型输入。文本附件发送真实文本，
// 图片附件发送真实 base64 图像；引用只发送已验证的稳定身份，不猜标题或伪造内容。
func loadPromptContentForRun(home, path string) ([]session.ContentBlock, string, []llm.MessageImage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", nil, attachmentBadRequest("读取 prompt content blocks 交接文件失败", err)
	}
	var blocks []transport.ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, "", nil, attachmentBadRequest("prompt content blocks 交接 JSON 损坏", err)
	}
	store := newAttachmentStore(home)
	resolved, err := store.ResolveBlocksForChild(blocks)
	if err != nil {
		return nil, "", nil, err
	}
	var text strings.Builder
	var images []llm.MessageImage
	for _, block := range resolved {
		switch block.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(block.Text)
		case "attachment":
			content, err := store.Get(block.AttachmentID)
			if err != nil {
				return nil, "", nil, err
			}
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			fmt.Fprintf(&text, "[附件 %s · %s · %s · %d bytes", content.Attachment.Name,
				content.Attachment.ID, content.Attachment.MIME, content.Attachment.ByteSize)
			if content.Attachment.Width > 0 {
				fmt.Fprintf(&text, " · %dx%d", content.Attachment.Width, content.Attachment.Height)
			}
			text.WriteByte(']')
			if content.Attachment.MIME == "text/plain" {
				text.WriteByte('\n')
				text.Write(content.Data)
			} else {
				images = append(images, llm.MessageImage{MIME: content.Attachment.MIME,
					DataBase64: base64.StdEncoding.EncodeToString(content.Data)})
			}
		case "reference":
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			if block.ReferenceType == "file" {
				fmt.Fprintf(&text, "[文件引用 %s · %s · %s]", block.Name, block.ReferenceID, block.Path)
			} else {
				fmt.Fprintf(&text, "[会话引用 %s · %s]", block.SessionID, block.ReferenceID)
			}
		}
	}
	return sessionBlocksFromTransport(resolved), text.String(), images, nil
}

// loadPromptPartsForRun 按 resolved 块的**原始顺序**构造模型输入：text 块、文本附件、
// 引用各贡献一段 TextPart，图片贡献一段 ImagePart（真实 base64）。
//
// 为什么不复用 loadPromptContentForRun 的 (text, images)：那一组返回值只能表达
// 「一整段文字 + 一串图片」。BEFORE/text + image + AFTER/text 会被压成
// "BEFORE\n[附件 …]\nAFTER" + 图片——图片夹在文字中间的位置丢失，模型看到的是
// "先读完两段文字再看图"。parts 按块逐段输出，顺序与持久化块一致。
//
// durable 块与旧 API 同源（同一个 ResolveBlocksForChild 结果），所以持久化不受影响；
// 旧函数保持原样，未接线前两者可并存。空文本块不产生段（旧 API 对空块也不写任何
// 字节）；未知块类型显式报错，绝不静默丢块——宁可发不出去，也不要发半个块。
func loadPromptPartsForRun(home, path string) ([]session.ContentBlock, []llm.MessagePart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, attachmentBadRequest("读取 prompt content blocks 交接文件失败", err)
	}
	var blocks []transport.ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return nil, nil, attachmentBadRequest("prompt content blocks 交接 JSON 损坏", err)
	}
	return promptPartsFromBlocks(home, blocks)
}

// promptPartsFromBlocks 是初始 content prompt、modelRunner steer 与 run IPC steer
// 共用的唯一块转换点：同一解析结果同时给 durable 块和有序模型 parts。
func promptPartsFromBlocks(home string, blocks []transport.ContentBlock) ([]session.ContentBlock, []llm.MessagePart, error) {
	store := newAttachmentStore(home)
	resolved, err := store.ResolveBlocksForChild(blocks)
	if err != nil {
		return nil, nil, err
	}
	parts := make([]llm.MessagePart, 0, len(resolved))
	for _, block := range resolved {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			parts = append(parts, llm.TextPart(block.Text))
		case "attachment":
			content, err := store.Get(block.AttachmentID)
			if err != nil {
				return nil, nil, err
			}
			if content.Attachment.MIME == "text/plain" {
				// 标识行与 loadPromptContentForRun 的附件文案逐字节一致（同一格式串；
				// 文本附件的 Width/Height 恒为 0，所以没有尺寸段），后面接真实文本。
				parts = append(parts, llm.TextPart(fmt.Sprintf("[附件 %s · %s · %s · %d bytes]\n%s",
					content.Attachment.Name, content.Attachment.ID, content.Attachment.MIME,
					content.Attachment.ByteSize, content.Data)))
				continue
			}
			parts = append(parts, llm.ImagePart(llm.MessageImage{MIME: content.Attachment.MIME,
				DataBase64: base64.StdEncoding.EncodeToString(content.Data)}))
		case "reference":
			switch block.ReferenceType {
			case "file":
				parts = append(parts, llm.TextPart(fmt.Sprintf("[文件引用 %s · %s · %s]",
					block.Name, block.ReferenceID, block.Path)))
			case "session":
				parts = append(parts, llm.TextPart(fmt.Sprintf("[会话引用 %s · %s]",
					block.SessionID, block.ReferenceID)))
			default:
				return nil, nil, attachmentBadRequest("未知 referenceType", nil)
			}
		default:
			return nil, nil, attachmentBadRequest("未知 content block type: "+block.Type, nil)
		}
	}
	return sessionBlocksFromTransport(resolved), parts, nil
}

var promptReferenceIDPattern = regexp.MustCompile(`^ref-[0-9a-f]{32}$`)

// ResolveBlocksForChild 验证父进程交接的完整块，但保留已分配的 referenceId。
// 客户端直接输入必须走 ResolveBlocks，不能借此传入伪造的元数据或引用 id。
func (s *attachmentStore) ResolveBlocksForChild(input []transport.ContentBlock) ([]transport.ContentBlock, error) {
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
			if !promptReferenceIDPattern.MatchString(block.ReferenceID) {
				return nil, attachmentBadRequest("子进程交接的 referenceId 形状非法", nil)
			}
			switch block.ReferenceType {
			case "file":
				abs, info, err := cleanAuthorizedPath(block.Path)
				if err != nil {
					return nil, err
				}
				out[i] = transport.ContentBlock{Type: "reference", ReferenceID: block.ReferenceID,
					ReferenceType: "file", Path: abs, Name: filepath.Base(abs), ByteSize: info.Size()}
			case "session":
				if block.SessionID == "" {
					return nil, attachmentBadRequest("session reference 缺 sessionId", nil)
				}
				if _, err := session.FindByID(s.home, block.SessionID); err != nil {
					return nil, attachmentNotFound("session reference 目标不存在", err)
				}
				out[i] = transport.ContentBlock{Type: "reference", ReferenceID: block.ReferenceID,
					ReferenceType: "session", SessionID: block.SessionID}
			default:
				return nil, attachmentBadRequest("未知 referenceType", nil)
			}
			meaningful = true
		default:
			return nil, attachmentBadRequest("未知 content block type", nil)
		}
	}
	if !meaningful {
		return nil, attachmentBadRequest("content 不能全是空文本", nil)
	}
	return out, nil
}
