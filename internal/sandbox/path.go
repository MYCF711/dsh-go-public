package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type pathClass int

const (
	pathOutside pathClass = iota
	pathWorkspace
	pathExtra
	pathHome
)

type resolver struct {
	workspace string
	extra     []string
	home      string
}

func newResolver(cfg Config) (resolver, error) {
	workspace, err := canonicalRoot("workspace", cfg.WorkspaceRoot)
	if err != nil {
		return resolver{}, err
	}
	home, err := canonicalRoot("home", cfg.HomeDir)
	if err != nil {
		return resolver{}, err
	}
	r := resolver{workspace: workspace, home: home}
	for _, root := range cfg.ExtraReadRoots {
		canonical, err := canonicalRoot("extra read", root)
		if err != nil {
			return resolver{}, err
		}
		r.extra = append(r.extra, canonical)
	}
	return r, nil
}

func canonicalRoot(label, root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%w: %s root 为空", ErrMissingContext, label)
	}
	canonical, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("%w: canonicalize %s root %q: %v", ErrInvalidRoot, label, root, err)
	}
	st, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: stat %s root %q: %v", ErrInvalidRoot, label, canonical, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%w: %s root %q 不是目录", ErrInvalidRoot, label, canonical)
	}
	return canonical, nil
}

// canonicalPath 解析最深已存在祖先的符号链接/reparse point，再拼回不存在后缀。
// 因此“已存在目标”和“经同一父目录新建目标”使用同一条 canonical 边界。
func canonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrInvalidPath)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: filepath.Abs(%q): %v", ErrInvalidPath, path, err)
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", fmt.Errorf("%w: EvalSymlinks(%q): %v", ErrInvalidPath, current, err)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("%w: Lstat(%q): %v", ErrInvalidPath, current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("%w: %q 没有已存在祖先", ErrInvalidPath, abs)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func (r resolver) resolve(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrMissingContext)
	}

	// 只有完整 ~ 或 ~/... 进入受控 home 通道；~other 不做 shell 式展开。
	if explicitHomePath(raw) {
		slashed := strings.ReplaceAll(raw, "\\", "/")
		rest := strings.TrimLeft(strings.TrimPrefix(slashed, "~"), "/")
		candidate := filepath.Join(r.home, filepath.FromSlash(rest))
		canonical, err := canonicalPath(candidate)
		if err != nil {
			return "", err
		}
		if !withinRoot(r.home, canonical) {
			return "", fmt.Errorf("%w: %q 越出 home %q", ErrPathOutsideHome, raw, r.home)
		}
		return canonical, nil
	}

	path := filepath.FromSlash(raw)
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.workspace, path)
	}
	return canonicalPath(path)
}

func explicitHomePath(raw string) bool {
	return raw == "~" || strings.HasPrefix(raw, "~/") || (os.PathSeparator == '\\' && strings.HasPrefix(raw, `~\`))
}

func (r resolver) classify(path string) pathClass {
	if withinRoot(r.workspace, path) {
		return pathWorkspace
	}
	for _, root := range r.extra {
		if withinRoot(root, path) {
			return pathExtra
		}
	}
	if withinRoot(r.home, path) {
		return pathHome
	}
	return pathOutside
}

// withinRoot 用 filepath.Rel 判断目录关系，不用字符串前缀，避免 workspace-evil 绕过。
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
