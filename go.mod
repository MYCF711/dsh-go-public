module dshgo

go 1.27.0

require (
	// zstd：标准库只有 gzip/flate，而 DSH 会话文件是**多帧 zstd**。
	// 选纯 Go 实现而不是 cgo 绑定：静态链接，CI 上不需要 C 工具链。
	github.com/klauspost/compress v1.20.3
	github.com/pelletier/go-toml/v2 v2.4.3
	github.com/tetratelabs/wazero v1.12.0
)

require golang.org/x/sys v0.44.0 // indirect
