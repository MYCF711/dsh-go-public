package v4

import "github.com/klauspost/compress/zstd"

// 仅供测试：暴露帧明文，用于核对真实文件的帧内格式。
func framePlaintexts(b []byte) ([][]byte, error) {
	frames, _, err := splitFrames(b)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	var out [][]byte
	for _, f := range frames {
		p, err := decodeFrame(dec, b[f.offset:f.offset+f.length])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
