package fsutil

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IgnoreFile 是仓库级"别扫我"声明。
const IgnoreFile = ".dshdignore"

// Ignore 是 .dshdignore 解析出来的匹配器。
//
// 为什么不复用 DefaultSkipDirs 的目录名集合：那个集合是**按名字**的，
// 只认 `refs` 这种通用名；而每个仓库里"该跳过的东西"是自己的事
// （实测：本仓 refs/dsc 里 3,569 个文件、9,600 个 vendored 测试函数，
// 一次误扫让"这个仓库有几个测试函数"从 252 变成 9,852）。
//
// 规则刻意做得比 .gitignore 简单：只有「路径前缀」和「glob」两种，
// 因为复杂的否定规则（`!`）没人能在脑子里算清，出错了更难查。
type Ignore struct {
	patterns []string
}

// LoadIgnore 读 <root>/.dshdignore。文件不存在不是错误。
func LoadIgnore(root string) (*Ignore, error) {
	f, err := os.Open(filepath.Join(root, IgnoreFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	ig := &Ignore{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 统一成正斜杠、去掉开头的 ./ 和 /，这样 pattern 与相对路径可比。
		line = strings.ReplaceAll(line, "\\", "/")
		line = strings.TrimPrefix(line, "./")
		line = strings.TrimPrefix(line, "/")
		if line != "" {
			ig.patterns = append(ig.patterns, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(ig.patterns) == 0 {
		return nil, nil
	}
	return ig, nil
}

// Empty 判断有没有实际规则。
func (ig *Ignore) Empty() bool { return ig == nil || len(ig.patterns) == 0 }

// Match 判断相对路径（正斜杠）是否被忽略。
//
// 一条 pattern 命中即忽略，判定顺序：
//  1. 完全相等；
//  2. 落在该目录下（pattern 是前缀目录）；
//  3. 按 glob 匹配（`*` 不跨目录，`**` 跨目录）。
func (ig *Ignore) Match(rel string) bool {
	if ig == nil || rel == "" {
		return false
	}
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	for _, p := range ig.patterns {
		dir := strings.TrimSuffix(p, "/")
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
		if ok, err := path.Match(p, rel); err == nil && ok {
			return true
		}
		if strings.Contains(p, "**") {
			if doubleStarMatch(p, rel) {
				return true
			}
		}
	}
	return false
}

// doubleStarMatch 处理 `**`：把它当成"任意层目录"。
//
// path.Match 不支持 **，而 `**/foo` 是写 .dshdignore 时最自然的写法。
func doubleStarMatch(pattern, rel string) bool {
	parts := strings.Split(pattern, "**")
	if len(parts) != 2 {
		// 多个 ** 的写法先不支持：与其猜错，不如按字面匹配。
		return false
	}
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")
	if prefix != "" && !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
		return false
	}
	rest := rel
	if prefix != "" {
		rest = strings.TrimPrefix(rel, prefix)
		rest = strings.TrimPrefix(rest, "/")
	}
	if suffix == "" {
		return true
	}
	// 后缀可能还带一层目录：逐个前缀尝试匹配。
	//
	// 还要认"目录被命中"的情况：`**/vendor` 既要命中 `a/vendor`，
	// 也要命中它下面的 `a/vendor/x.go`——声明者的意思是"这个目录别扫"。
	for {
		if ok, err := path.Match(suffix, rest); err == nil && ok {
			return true
		}
		if rest == suffix || strings.HasPrefix(rest, suffix+"/") {
			return true
		}
		i := strings.Index(rest, "/")
		if i < 0 {
			return false
		}
		rest = rest[i+1:]
	}
}

// WalkOptions 把遍历策略收在一处。零值可用。
type WalkOptions struct {
	SkipDirs map[string]bool // 按目录名的通用跳过表；nil 用 DefaultSkipDirs
	Ignore   *Ignore         // 仓库级声明；nil 表示没有
	Root     string          // 计算相对路径用的根；空则用遍历起点
	// Skipped 非 nil 时，被目录名规则跳过的条目数写进来。
	Skipped *int
	// Ignored 非 nil 时，被 .dshdignore 跳过的条目数写进来。
	Ignored *int
}
