// Package v4 是 DSH 会话文件格式（v4）的编解码。
//
// 它是**边界契约**：内核只认 internal/session 的 canonical 事件，
// 这个包负责把磁盘上的字节翻译成事件、把事件翻译回字节。拔掉它不需要改内核一行。
//
// 物理格式（实测，见 research/21）：
//   - 文件名 session.v4.jsonl.zstd，内容是**多个独立 zstd 帧的拼接**；
//   - 第 1 帧只装 header 一行，必须恰好一行且以 \n 结尾（list/stat 只解首帧）；
//   - 之后每个 durable 批次一帧，帧内是多行 JSON，一行一个事件；
//   - 只开 checksum、不设 level；追加不重写已提交帧。
package v4

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// FrameMagic 是 zstd 帧魔数（字节 28 B5 2F FD）。
const FrameMagic uint32 = 0xFD2FB528

// frame 是一帧在文件里的位置。
type frame struct {
	offset int
	length int
}

// splitFrames 只解析帧结构，不解压——帧边界必须精确，否则「首帧只装 header」这条
// 契约就无从验证。
func splitFrames(b []byte) (frames []frame, truncatedAt int, err error) {
	var out []frame
	pos := 0
	for pos < len(b) {
		start := pos
		if len(b)-pos < 5 {
			return out, pos, nil
		}
		if binary.LittleEndian.Uint32(b[pos:]) != FrameMagic {
			return nil, 0, fmt.Errorf("偏移 %d 不是 zstd 帧魔数", pos)
		}
		pos += 4

		fhd := b[pos]
		pos++
		singleSegment := fhd&0x20 != 0
		checksum := fhd&0x04 != 0
		dictIDFlag := fhd & 0x03
		fcsFlag := (fhd >> 6) & 0x03

		if !singleSegment { // Window_Descriptor
			pos++
		}
		switch dictIDFlag {
		case 1:
			pos += 1
		case 2:
			pos += 2
		case 3:
			pos += 4
		}
		fcsSize := 0
		switch fcsFlag {
		case 0:
			if singleSegment {
				fcsSize = 1
			}
		case 1:
			fcsSize = 2
		case 2:
			fcsSize = 4
		case 3:
			fcsSize = 8
		}
		pos += fcsSize
		if pos > len(b) {
			return out, start, nil
		}

		for {
			if pos+3 > len(b) {
				return out, start, nil
			}
			bh := uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16
			pos += 3
			last := bh&1 == 1
			blockType := (bh >> 1) & 0x03
			size := int(bh >> 3)
			switch blockType {
			case 0, 2: // raw / compressed
				pos += size
			case 1: // RLE
				pos += 1
			default:
				return nil, 0, fmt.Errorf("偏移 %d 处出现保留块类型", pos-3)
			}
			if pos > len(b) {
				return out, start, nil
			}
			if last {
				break
			}
		}
		if checksum {
			pos += 4
		}
		if pos > len(b) {
			return out, start, nil
		}
		out = append(out, frame{offset: start, length: pos - start})
	}
	return out, 0, nil
}

// decodeFrame 解一帧的明文。
func decodeFrame(dec *zstd.Decoder, b []byte) ([]byte, error) {
	out, err := dec.DecodeAll(b, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd 解帧失败: %w", err)
	}
	return out, nil
}
