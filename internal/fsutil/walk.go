// Package fsutil 是文件遍历的小工具，零业务、零依赖。
//
// 它不属于 SPI（SPI 只放接口），也不属于内核（内核不认识文件系统），
// 放在这里是为了让 glob / grep 这两个工具共享同一套「走哪些文件」的规则。
package fsutil

import (
	"io/fs"
	"path/filepath"
	"strings"
)

// DefaultSkipDirs 是遍历时一律跳过的目录名。
//
// 依据实测：本仓工作区里 .git / node_modules / refs 占了绝大多数文件，
// 全量遍历一次要几十万条目，而模型真正关心的不到千分之一。
var DefaultSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".dsh-zhuli":   true,
	"refs":         true,
	"dist":         true,
	"bin":          true,
	"tmp":          true,
}

// WalkFiles 遍历 root 下的普通文件，跳过 skipDirs 里出现的目录名。
// 读不了的目录/条目直接跳过（尽力而为），不中断整次遍历。
func WalkFiles(root string, skipDirs map[string]bool, fn func(abs string) error) error {
	return WalkFilesWith(root, WalkOptions{SkipDirs: skipDirs}, fn)
}

// WalkFilesWith 是带仓库级忽略声明的遍历。
//
// Ignore 按**相对 root 的正斜杠路径**匹配，所以调用方要把 root 传稳
// （工具里就是工作区根），否则 .dshdignore 里的 `refs/` 会匹配不上。
func WalkFilesWith(root string, opts WalkOptions, fn func(abs string) error) error {
	skipDirs := opts.SkipDirs
	if skipDirs == nil {
		skipDirs = DefaultSkipDirs
	}
	base := opts.Root
	if base == "" {
		base = root
	}
	base = filepath.ToSlash(filepath.Clean(base))

	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root {
				return filepath.SkipDir
			}
			return nil
		}

		rel := ""
		if slash := filepath.ToSlash(p); strings.HasPrefix(slash, base+"/") {
			rel = slash[len(base)+1:]
		}

		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				if opts.Skipped != nil {
					*opts.Skipped++
				}
				return filepath.SkipDir
			}
			// 目录本身就命中忽略规则：直接剪枝，不必进去数文件。
			if rel != "" && opts.Ignore.Match(rel) {
				if opts.Ignored != nil {
					*opts.Ignored++
				}
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if rel != "" && opts.Ignore.Match(rel) {
			if opts.Ignored != nil {
				*opts.Ignored++
			}
			return nil
		}
		return fn(p)
	})
}

// IsBinary 用「前 8KB 里有没有 NUL」判断二进制。
func IsBinary(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}
