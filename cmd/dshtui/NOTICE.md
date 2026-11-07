# 来源与许可

`cmd/dshtui` 的开屏点阵字体取自 **dsh-TUI**（MIT）：

- 项目：https://github.com/ccch1mneyyy/dsh-TUI
- 文件：`src/components/splashFonts.ts`（`BOLD_GLYPHS` 与 `BOLD_FALLBACK`）
- 版权：Copyright (c) 2026 chimney (ccch1mneyyy)
- 许可：MIT License

搬运方式：`scripts/import-splash.py` 机械解析源文件并生成 `splash_gen.go`，
**不手抄**——5 行点阵手抄错一格，看起来只会像"这个字母有点怪"，是最难发现的错。
要更新字体，改上游源文件再重新生成，不要直接改 `splash_gen.go`。

MIT 要求保留版权声明与许可文本；本目录下的生成文件头部已带归属注释，
完整许可见上游仓库的 `LICENSE`。
