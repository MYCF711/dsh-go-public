package modes

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// fileMode 是磁盘上 `$DSH_HOME/modes/*.toml` 的形状。
type fileMode struct {
	ID          string   `toml:"id"`
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	CopyOf      string   `toml:"copy_of"`
	Tools       []string `toml:"tools"`
	Approval    string   `toml:"approval"`
	Sandbox     string   `toml:"sandbox"`
	Builtin     bool     `toml:"builtin"`
}

// LoadDir 读一个目录下的用户自建模式。
//
// 三条校验（PLAN §6）：
//  1. 不能用内置 id —— 内置模式只读；
//  2. 不能自己声明 `builtin = true` —— 内置是编译期属性，不是配置项；
//  3. `copy_of` 指向的模式必须存在（从它继承工具白名单）。
//
// 目录不存在返回空集、不报错：大多数用户没有 modes 目录。
func LoadDir(dir string) ([]Mode, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读模式目录 %s 失败: %w", dir, err)
	}

	seen := map[string]string{} // id → 来源文件
	out := make([]Mode, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读 %s 失败: %w", path, err)
		}
		var fm fileMode
		if err := toml.Unmarshal(raw, &fm); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		if fm.ID == "" {
			return nil, fmt.Errorf("%s 缺 id", path)
		}
		if fm.Builtin {
			return nil, fmt.Errorf("%s 声明了 builtin = true；内置是编译期属性，用户模式不能自称内置", path)
		}
		if err := OverrideBuiltin(fm.ID); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if prev, dup := seen[fm.ID]; dup {
			return nil, fmt.Errorf("模式 id %q 冲突：%s 与 %s 都用了它", fm.ID, prev, path)
		}
		seen[fm.ID] = path

		m := Mode{
			ID:          fm.ID,
			Name:        firstNonEmpty(fm.Name, fm.ID),
			Description: fm.Description,
			Tools:       fm.Tools,
			Approval:    fm.Approval,
			Sandbox:     fm.Sandbox,
			Builtin:     false,
		}
		if fm.CopyOf != "" {
			base, err := Resolve(fm.CopyOf)
			if err != nil {
				return nil, fmt.Errorf("%s: copy_of %q 不存在", path, fm.CopyOf)
			}
			if len(m.Tools) == 0 {
				m.Tools = append([]string(nil), base.Tools...)
			}
			if m.Approval == "" {
				m.Approval = base.Approval
			}
			if m.Sandbox == "" {
				m.Sandbox = base.Sandbox
			}
		}
		// 用户模式的 sandbox 字段必须在装配期就是真值；未知值直接报错，
		// 不能等到第一次工具调用才发现（那时模型已经看到工具表了）。
		if _, err := m.SandboxMode(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Registry 是"内置 + 用户自建"的合集。
type Registry struct {
	builtins []Mode
	user     []Mode
}

// NewRegistry 造一个注册表。用户模式里出现内置 id 会报错（不静默覆盖）。
func NewRegistry(user []Mode) (*Registry, error) {
	for _, m := range user {
		if err := OverrideBuiltin(m.ID); err != nil {
			return nil, err
		}
	}
	return &Registry{builtins: Builtins(), user: user}, nil
}

// Resolve 按 id 找模式。用户模式优先查——内置 id 已在 NewRegistry 挡掉，不会撞。
func (r *Registry) Resolve(id string) (Mode, error) {
	if id == "" {
		id = DefaultID
	}
	for _, m := range r.user {
		if m.ID == id {
			return m, nil
		}
	}
	for _, m := range r.builtins {
		if m.ID == id {
			return m, nil
		}
	}
	return Mode{}, fmt.Errorf("未知模式 %q（可选：%s）", id, r.IDs())
}

// All 返回全部模式（内置在前）。
func (r *Registry) All() []Mode {
	out := make([]Mode, 0, len(r.builtins)+len(r.user))
	out = append(out, r.builtins...)
	out = append(out, r.user...)
	return out
}

// IDs 列出全部 id。
func (r *Registry) IDs() string {
	ms := r.All()
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.ID)
	}
	return strings.Join(parts, ", ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
