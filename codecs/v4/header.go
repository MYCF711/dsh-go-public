package v4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// ReadHeader 只解第一帧（header），不碰后面的正文。
//
// 列会话只需要 id / cwd / createdAt，全量解码一份大会话要几十毫秒。
// header 有单独一帧，这是 v4 格式本身保证的（第 1 帧只装 header）。
//
// 实现上连**读**也省：按块读，凑够第一帧就停，大会话不会被整个读进内存。
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("读会话文件失败: %w", err)
	}
	defer f.Close()

	const startSize = 64 << 10
	buf := make([]byte, 0, startSize)
	chunk := make([]byte, startSize)
	for {
		n, rerr := f.Read(chunk)
		buf = append(buf, chunk[:n]...)

		frames, _, serr := splitFrames(buf)
		if serr != nil {
			return Header{}, fmt.Errorf("%s: %w", path, serr)
		}
		if len(frames) > 0 {
			h, herr := headerFromFrame(buf[frames[0].offset : frames[0].offset+frames[0].length])
			if herr != nil {
				return Header{}, fmt.Errorf("%s: %w", path, herr)
			}
			return h, nil
		}

		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Header{}, fmt.Errorf("读会话文件失败: %w", rerr)
		}
	}
	return Header{}, fmt.Errorf("%s: 文件里没有 zstd 帧", path)
}

// HeaderFrom 从**整个文件**的字节里只解首帧（不再碰后面的帧）。
// 与 Read 共用同一套首帧校验，保证「列会话看到的」与「打开会话看到的」不分叉。
func HeaderFrom(b []byte) (Header, error) {
	frames, _, err := splitFrames(b)
	if err != nil {
		return Header{}, err
	}
	if len(frames) == 0 {
		return Header{}, fmt.Errorf("文件里没有 zstd 帧")
	}
	return headerFromFrame(b[frames[0].offset : frames[0].offset+frames[0].length])
}

// headerFromFrame 解一帧并要求它是合法的 header。
func headerFromFrame(frameBytes []byte) (Header, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return Header{}, err
	}
	defer dec.Close()

	head, err := decodeFrame(dec, frameBytes)
	if err != nil {
		return Header{}, fmt.Errorf("首帧: %w", err)
	}
	return parseHeader(head)
}

// parseHeader 校验并解析首帧明文。Read 与 ReadHeader 都走这里。
func parseHeader(head []byte) (Header, error) {
	var h Header
	// 契约：首帧只装 header 一行，且以 \n 结尾（list/stat 只解首帧，靠这条跳过整个 body）。
	if !bytes.HasSuffix(head, []byte("\n")) {
		return h, fmt.Errorf("首帧明文没有以 \\n 结尾")
	}
	body := head[:len(head)-1]
	if bytes.ContainsRune(body, '\n') {
		return h, fmt.Errorf("首帧含 %d 行，契约要求恰好 1 行（header 必须独立成帧）", bytes.Count(head, []byte("\n")))
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return h, fmt.Errorf("解析 header: %w", err)
	}
	if h.Type != "session" {
		return h, fmt.Errorf("首行 type = %q，期望 session", h.Type)
	}
	if h.Version != 4 {
		return h, fmt.Errorf("版本 = %d，本解码器只认 4", h.Version)
	}
	return h, nil
}
