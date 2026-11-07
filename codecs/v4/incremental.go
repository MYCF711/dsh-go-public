package v4

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ReadSince 从文件的某个字节偏移往后读，返回新事件和下次该从哪读。
//
// 为什么需要它：`ReadFile` 每次都从头解压整份日志。
// 实测一个 2.2 万事件的会话单次 ReadFile 要 **300 ms**；
// 边跑边看的循环每 200 ms 读一次，一轮 20 秒就是 100 次 = **30 秒 CPU**，
// 比它监看的那一轮还慢——读得越久落后越多。
//
// 语义：
//   - offset = 0 等价于读全部
//   - offset 落在半帧上（文件正在追加）→ 返回已读到的部分，next 停在完整的帧边界
//   - 文件变短（被重建）→ 从头读
//
// 返回的 next 是**下一个安全起点**，不是文件长度：只把完整解开的帧算进去。
// Reader 复用同一个 zstd 解码器。
//
// 为什么要复用一个：`follow` 每 200ms 读一次，一轮 20 秒就是 100 次。
// 每次都 NewReader 会让解码器缓冲区反复申请——实测 serve 长跑时
// Sys 以约 19KB/轮 单调增长，怀疑就是它。**这是待验证的怀疑，不是结论。**
type Reader struct {
	dec *zstd.Decoder
}

func NewReader() (*Reader, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &Reader{dec: dec}, nil
}

func (r *Reader) Close() {
	if r != nil && r.dec != nil {
		r.dec.Close()
	}
}

// ReadSince 是复用解码器版本，语义与包级 ReadSince 完全一致。
func (r *Reader) ReadSince(path string, offset int64) ([]Event, int64, error) {
	return readSince(r.dec, path, offset)
}

// ReadSince 一次性读：内部借一个解码器，读完关掉。
func ReadSince(path string, offset int64) (events []Event, next int64, err error) {
	r, err := NewReader()
	if err != nil {
		return nil, offset, err
	}
	defer r.Close()
	return r.ReadSince(path, offset)
}

func readSince(dec *zstd.Decoder, path string, offset int64) (events []Event, next int64, err error) {
	// 只读尾巴：从 offset 处 seek 进去读剩下的。
	//
	// 原来这里是 os.ReadFile(path) 再切片——省了 CPU（不解压前面的帧），
	// 但**没省内存**：每次都把整份文件读进内存，会话越长分配越大。
	// serve 长跑里 Sys 每轮涨二三十 KB，来源就是它。
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()

	if st, serr := f.Stat(); serr == nil && (offset < 0 || offset > st.Size()) {
		offset = 0
	}
	if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
		return nil, offset, serr
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, offset, err
	}
	frames, _, err := splitFrames(b)
	if err != nil {
		return nil, offset, err
	}
	for _, f := range frames {
		raw, eerr := decodeFrame(dec, b[f.offset:f.offset+f.length])
		if eerr != nil {
			// 帧坏了就停在这帧之前，下次从同一处重试，不要吞掉后面的。
			break
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			// header 那一帧的类型是 session，不是事件；跳过它。
			if ev.Type == "session" {
				continue
			}
			events = append(events, ev)
		}
		next = offset + int64(f.offset) + int64(f.length)
	}
	if next == 0 {
		next = offset
	}
	return events, next, nil
}

// ReadTail 只解最后 frames 帧，返回其中的事件。
//
// 为什么需要：`session.load` 只要首屏 50 条，但为了给出准确的 total，
// 传统做法是把 13,900 帧全解出来（实测 138 ms 解压 + 200 ms 投影）。
// 帧之间互相独立（每帧自带 magic），所以从尾部挑几帧解是可行的——实测 0.5 ms。
//
// **它拿不出 total。** 要数总条数就必须解完全部，这是契约层面的取舍，
// 见 docs/transport-obligations.md 的「total 可以迟到」。
func ReadTail(path string, frames int) ([]Event, error) {
	if frames <= 0 {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	starts := frameStarts(b)
	if len(starts) == 0 {
		return nil, nil
	}
	dec, derr := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if derr != nil {
		return nil, derr
	}
	defer dec.Close()

	// 按帧边界逐段解：最后几帧是连续的一段字节，每段独立。
	var out []Event
	for k := len(starts) - frames; k < len(starts); k++ {
		if k < 0 {
			continue
		}
		end := len(b)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		raw, eerr := decodeFrame(dec, b[starts[k]:end])
		if eerr != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev Event
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if ev.Type == "session" {
				continue
			}
			out = append(out, ev)
		}
	}
	return out, nil
}

// frameStarts 找出所有帧起点（zstd magic 的位置）。
func frameStarts(b []byte) []int {
	var magic = []byte{0x28, 0xb5, 0x2f, 0xfd}
	var out []int
	for i := 0; i+4 <= len(b); i++ {
		if b[i] == magic[0] && b[i+1] == magic[1] && b[i+2] == magic[2] && b[i+3] == magic[3] {
			out = append(out, i)
		}
	}
	return out
}
