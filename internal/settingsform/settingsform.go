// Package settingsform 实现官方 ConfigForm 的命名空间描述、路径写入与 revision 乐观锁。
// 存储由宿主注入；本包不另建配置文件，也不知道 HTTP 或会话日志。
package settingsform

import (
	"errors"
	"fmt"
	"strings"
)

const (
	NamespacePermission = "permission"
	NamespaceModel      = "model"
	FieldDefaultPreset  = "defaultPreset"
	FieldDefaultModel   = "defaultModel"
)

// Error 是 Transport 四字段错误本身；Status 只用于 HTTP，不序列化进 error 对象。
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Hint      string `json:"hint"`
	Retryable bool   `json:"retryable"`
	Status    int    `json:"-"`
}

func (e *Error) Error() string { return e.Message }

// AsError 保留已分类错误；其它存储/序列化失败一律是 fail-loud internal。
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return &Error{Code: "internal", Message: err.Error(), Hint: "检查宿主存储与 dshd stderr 后可重试", Retryable: true, Status: 500}
}

// RevisionConflict 在读取时与存储原子提交点共用同一错误形状。
func RevisionConflict(ns string, expected, actual int64) *Error {
	return &Error{
		Code:    "revision_conflict",
		Message: fmt.Sprintf("%s revision 已变化：expected=%d actual=%d，未覆盖新值", ns, expected, actual),
		Hint:    "重新 settings.describe/get，基于最新 revision 再提交",
		Status:  409,
	}
}

// Document 是一个命名空间的持久层。Base 来自现有启动默认；User 只含显式覆盖。
// Revision 必须与 User 同存储、同一次原子写提交。
type Document struct {
	Base     map[string]any
	User     map[string]any
	Revision int64
}

// Store 的实现必须在 Write 内原子检查 expectedRevision 并持久化 user/revision。
// 写失败不得把新值留在可读内存里，否则调用方会看到假“保存成功”。
type Store interface {
	Read(namespace string) (Document, error)
	Write(namespace string, user map[string]any, expectedRevision *int64) (int64, error)
}

// PathOp 与官方 SettingsPathOpView 同形：set 带 value，unset 不带。
type PathOp struct {
	Op    string   `json:"op"`
	Path  []string `json:"path"`
	Value any      `json:"value,omitempty"`
}

// NamespaceView 是官方 ConfigForm describe/mutate 的线上描述符。
type NamespaceView struct {
	NS           string         `json:"ns"`
	Schema       any            `json:"schema"`
	Value        map[string]any `json:"value"`
	Base         map[string]any `json:"base"`
	User         map[string]any `json:"user,omitempty"`
	AutoGenerate bool           `json:"autoGenerate"`
	Applies      string         `json:"applies"`
	Secrets      []string       `json:"secrets"`
	Revision     int64          `json:"revision"`
}

// SecretSlot 是官方 `SettingsNamespaceView.secrets` 的一项：路径 + 是否已配置。
//
// 与 NamespaceView.Secrets（路径字符串数组）的区别来自官方 zod 契约：
// 官方是 `{path: string[], set: boolean}` 的对象数组。既有
// settings.describe/mutate 的线形状是本仓 58 个方法之一，不能改；官方
// 新方法（settings.update/replace）走 Official() 投影说话。
type SecretSlot struct {
	Path []string `json:"path"`
	Set  bool     `json:"set"`
}

// OfficialNamespaceView 是官方 settings/update|replace 的结果投影，字段逐条
// 对齐官方 zod：ns/autoGenerate/schema/value/applies/secrets/revision 必填，
// base/user 可选。
type OfficialNamespaceView struct {
	NS           string         `json:"ns"`
	AutoGenerate bool           `json:"autoGenerate"`
	Schema       any            `json:"schema"`
	Value        map[string]any `json:"value"`
	Base         map[string]any `json:"base,omitempty"`
	User         map[string]any `json:"user,omitempty"`
	Applies      string         `json:"applies"`
	Secrets      []SecretSlot   `json:"secrets"`
	Revision     int64          `json:"revision"`
}

// Official 把既有描述符投影成官方形状。**只投影，不重算**：值、base、user、
// revision 都来自同一份 Document 的同一次读取。
func (v NamespaceView) Official() OfficialNamespaceView {
	secrets := make([]SecretSlot, 0, len(v.Secrets))
	for _, path := range v.Secrets {
		key := path
		secrets = append(secrets, SecretSlot{
			Path: []string{key},
			Set:  holdsValue(v.User, key) || holdsValue(v.Base, key),
		})
	}
	return OfficialNamespaceView{
		NS: v.NS, AutoGenerate: v.AutoGenerate, Schema: v.Schema,
		Value: v.Value, Base: v.Base, User: v.User, Applies: v.Applies,
		Secrets: secrets, Revision: v.Revision,
	}
}

// holdsValue 判断一个 secret 槽位当前是否有值。只看"有没有"，从不取值出来——
// 官方该字段叫 set，值本身从不上线。
func holdsValue(section map[string]any, key string) bool {
	if section == nil {
		return false
	}
	value, ok := section[key]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return true
	}
}

// Field 是通用机器可读表单投影；current/default 都来自同一份 Document。
type Field struct {
	Name       string   `json:"name"`
	Path       []string `json:"path"`
	Type       string   `json:"type"`
	Current    any      `json:"current"`
	Default    any      `json:"default"`
	Overridden bool     `json:"overridden"`
	Writable   bool     `json:"writable"`
	Enum       []string `json:"enum,omitempty"`
	MinLength  *int     `json:"minLength,omitempty"`
	MaxLength  *int     `json:"maxLength,omitempty"`
}

// Section 是一个命名空间的表单投影。
type Section struct {
	NS       string  `json:"ns"`
	Title    string  `json:"title"`
	Writable bool    `json:"writable"`
	Revision int64   `json:"revision"`
	Fields   []Field `json:"fields"`
	Errors   []Error `json:"errors"`
}

// DescribeResult 同时携带官方 namespaces 与通用 sections，旧客户端可忽略后者。
type DescribeResult struct {
	Writable    bool            `json:"writable"`
	HasDocument bool            `json:"hasDocument"`
	Namespaces  []NamespaceView `json:"namespaces"`
	Sections    []Section       `json:"sections"`
	Errors      []Error         `json:"errors"`
}

// MutationResult 让官方 mutate 与 form.set/reset 共用同一次提交结果。
type MutationResult struct {
	Namespace NamespaceView
	Section   Section
}

type fieldSpec struct {
	name      string
	enum      []string
	minLength *int
	maxLength *int
}

type sectionSpec struct {
	ns     string
	title  string
	fields []fieldSpec
}

// Service 是无网络、无文件系统副作用的 ConfigForm 核心。
type Service struct {
	store Store
	specs []sectionSpec
}

// New 固定首切的两个真实宿主设置；其它 namespace 不会靠空 schema 假装可用。
func New(store Store) *Service {
	one, max := 1, 256
	return &Service{store: store, specs: []sectionSpec{
		{ns: NamespaceModel, title: "Default model", fields: []fieldSpec{{name: FieldDefaultModel, minLength: &one, maxLength: &max}}},
		{ns: NamespacePermission, title: "Default permission", fields: []fieldSpec{{name: FieldDefaultPreset, enum: []string{"deny", "ask", "allow", "auto"}}}},
	}}
}

func (s *Service) spec(namespace string) (sectionSpec, error) {
	for _, spec := range s.specs {
		if spec.ns == namespace {
			return spec, nil
		}
	}
	return sectionSpec{}, &Error{Code: "not_found", Message: "未知设置 namespace: " + namespace, Hint: "只使用 settings.describe 返回的 ns", Status: 404}
}

func (s *Service) read(namespace string) (sectionSpec, Document, error) {
	spec, err := s.spec(namespace)
	if err != nil {
		return sectionSpec{}, Document{}, err
	}
	if s.store == nil {
		return sectionSpec{}, Document{}, &Error{Code: "internal", Message: "settings store 未接线", Hint: "检查 dshd serve 装配", Retryable: true, Status: 500}
	}
	doc, err := s.store.Read(namespace)
	if err != nil {
		return sectionSpec{}, Document{}, AsError(err)
	}
	if doc.Base == nil {
		return sectionSpec{}, Document{}, &Error{Code: "internal", Message: namespace + " 存储缺少 base", Hint: "检查现有默认设置接线", Retryable: true, Status: 500}
	}
	if doc.Revision < 0 {
		return sectionSpec{}, Document{}, &Error{Code: "internal", Message: namespace + " revision 不能为负数", Hint: "检查持久设置文件", Retryable: true, Status: 500}
	}
	if err := validateUser(spec, doc.Base); err != nil {
		return sectionSpec{}, Document{}, &Error{Code: "internal", Message: "启动默认值无效: " + err.Error(), Hint: "修正 serve 的 --model/--policy 或既有存储", Retryable: true, Status: 500}
	}
	if err := validateUser(spec, doc.User); err != nil {
		return sectionSpec{}, Document{}, &Error{Code: "internal", Message: "持久默认值无效: " + err.Error(), Hint: "修正既有 session override 文件中的保留默认值", Retryable: true, Status: 500}
	}
	return spec, doc, nil
}

// Describe 读取所有真实命名空间；任何一个存储坏掉都整体报错，不拿旧值冒充现状。
func (s *Service) Describe() (DescribeResult, error) {
	out := DescribeResult{
		Writable: true, HasDocument: false,
		Namespaces: make([]NamespaceView, 0, len(s.specs)),
		Sections:   make([]Section, 0, len(s.specs)),
		Errors:     make([]Error, 0),
	}
	for _, spec := range s.specs {
		_, doc, err := s.read(spec.ns)
		if err != nil {
			return DescribeResult{}, err
		}
		view := makeView(spec, doc)
		out.Namespaces = append(out.Namespaces, view.Namespace)
		out.Sections = append(out.Sections, view.Section)
	}
	return out, nil
}

// Get 返回一个命名空间的机器可读 section。
func (s *Service) Get(namespace string) (Section, error) {
	spec, doc, err := s.read(namespace)
	if err != nil {
		return Section{}, err
	}
	return makeView(spec, doc).Section, nil
}

// Mutate 在一次 revision 栅栏后按顺序应用 ops；任何 op 失败都不写存储。
func (s *Service) Mutate(namespace string, ops []PathOp, expectedRevision *int64) (MutationResult, error) {
	if expectedRevision != nil && *expectedRevision < 0 {
		return MutationResult{}, &Error{Code: "bad_request", Message: "expectedRevision 不能为负数", Hint: "省略它或使用 describe/get 返回的非负 revision", Status: 400}
	}
	spec, doc, err := s.read(namespace)
	if err != nil {
		return MutationResult{}, err
	}
	if expectedRevision != nil && *expectedRevision != doc.Revision {
		return MutationResult{}, RevisionConflict(namespace, *expectedRevision, doc.Revision)
	}
	user := clone(doc.User)
	if user == nil {
		user = map[string]any{}
	}
	for _, op := range ops {
		field, err := spec.field(op.Path)
		if err != nil {
			return MutationResult{}, err
		}
		switch op.Op {
		case "set":
			user[field.name] = op.Value
		case "unset":
			delete(user, field.name)
		default:
			return MutationResult{}, &Error{Code: "bad_request", Message: "未知设置 op: " + op.Op, Hint: "op 只能是 set 或 unset", Status: 400}
		}
	}
	if err := validateUser(spec, user); err != nil {
		return MutationResult{}, err
	}
	revision, err := s.store.Write(namespace, user, expectedRevision)
	if err != nil {
		return MutationResult{}, AsError(err)
	}
	doc.User = user
	doc.Revision = revision
	return makeView(spec, doc), nil
}

// Update 是官方 `settings/update` 的语义：把 patch **合并**进一个 namespace 的
// user 段（没提到的键保持原样）。
//
// 与 Mutate 的区别只在输入形状（官方是 `z.record(string, json)` 的顶层键值），
// 提交路径完全同一条：读新文档 → revision 栅栏 → 校验 → **一次** 原子写。
// 任何一步失败都不留痕：读路径没有缓存，写失败由 Store 自己回滚内存旧值，
// 所以这里不存在"内存显示成功、磁盘还是旧值"的中间态。
func (s *Service) Update(namespace string, patch map[string]any, expectedRevision *int64) (MutationResult, error) {
	return s.writeSection(namespace, patch, expectedRevision, false)
}

// Replace 是官方 `settings/replace` 的语义：user 段**整段替换**成 section。
// 空 section 等于清空 user 段（值回落到 base），这也是一次真实写入（revision+1）。
func (s *Service) Replace(namespace string, section map[string]any, expectedRevision *int64) (MutationResult, error) {
	return s.writeSection(namespace, section, expectedRevision, true)
}

func (s *Service) writeSection(namespace string, input map[string]any, expectedRevision *int64, replace bool) (MutationResult, error) {
	if expectedRevision != nil && *expectedRevision < 0 {
		return MutationResult{}, &Error{Code: "bad_request", Message: "expectedRevision 不能为负数", Hint: "省略它或使用 describe/get 返回的非负 revision", Status: 400}
	}
	spec, doc, err := s.read(namespace)
	if err != nil {
		return MutationResult{}, err
	}
	if expectedRevision != nil && *expectedRevision != doc.Revision {
		return MutationResult{}, RevisionConflict(namespace, *expectedRevision, doc.Revision)
	}
	var user map[string]any
	if replace {
		user = make(map[string]any, len(input))
	} else {
		user = clone(doc.User)
		if user == nil {
			user = map[string]any{}
		}
	}
	for key, value := range input {
		user[key] = value
	}
	// 整段校验先于写：未知字段/类型/枚举/长度任一不合就整次拒绝，
	// 不写半份（这也是"失败回滚"最简单的实现：根本没写）。
	if err := validateUser(spec, user); err != nil {
		return MutationResult{}, err
	}
	revision, err := s.store.Write(namespace, user, expectedRevision)
	if err != nil {
		return MutationResult{}, AsError(err)
	}
	doc.User = user
	doc.Revision = revision
	return makeView(spec, doc), nil
}

// Reset 清一个字段（path 为单段）或整个 namespace（path 为 nil）。
func (s *Service) Reset(namespace string, path []string, expectedRevision *int64) (MutationResult, error) {
	if path == nil {
		spec, err := s.spec(namespace)
		if err != nil {
			return MutationResult{}, err
		}
		ops := make([]PathOp, 0, len(spec.fields))
		for _, field := range spec.fields {
			ops = append(ops, PathOp{Op: "unset", Path: []string{field.name}})
		}
		return s.Mutate(namespace, ops, expectedRevision)
	}
	return s.Mutate(namespace, []PathOp{{Op: "unset", Path: path}}, expectedRevision)
}

func (s sectionSpec) field(path []string) (fieldSpec, error) {
	if len(path) != 1 || strings.TrimSpace(path[0]) == "" {
		return fieldSpec{}, &Error{Code: "invalid_path", Message: fmt.Sprintf("设置路径必须是单段非空字段，收到 %v", path), Hint: "使用 describe 中 field.path 的原样单段路径", Status: 400}
	}
	for _, field := range s.fields {
		if field.name == path[0] {
			return field, nil
		}
	}
	return fieldSpec{}, &Error{Code: "unknown_field", Message: fmt.Sprintf("%s 没有字段 %q", s.ns, path[0]), Hint: "只修改 settings.describe 返回的字段", Status: 400}
}

func validateUser(spec sectionSpec, user map[string]any) error {
	for key := range user {
		if _, err := spec.field([]string{key}); err != nil {
			return err
		}
	}
	for _, field := range spec.fields {
		value, ok := user[field.name]
		if !ok {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return &Error{Code: "invalid_type", Message: fmt.Sprintf("%s.%s 必须是 string，收到 %T", spec.ns, field.name, value), Hint: "按 field.type 提交 JSON 字符串", Status: 400}
		}
		if len(field.enum) > 0 {
			valid := false
			for _, candidate := range field.enum {
				if text == candidate {
					valid = true
					break
				}
			}
			if !valid {
				return &Error{Code: "invalid_enum", Message: fmt.Sprintf("%s.%s 的值 %q 不在 %v 中", spec.ns, field.name, text, field.enum), Hint: "只提交 field.enum 原样列出的值", Status: 400}
			}
		}
		length := len([]byte(text))
		if field.minLength != nil && (length < *field.minLength || strings.TrimSpace(text) == "") {
			return &Error{Code: "out_of_range", Message: fmt.Sprintf("%s.%s 长度必须至少 %d 字节且不能为空白", spec.ns, field.name, *field.minLength), Hint: "缩短或补全模型 id 后重试", Status: 400}
		}
		if field.maxLength != nil && length > *field.maxLength {
			return &Error{Code: "out_of_range", Message: fmt.Sprintf("%s.%s 长度 %d 超过上限 %d 字节", spec.ns, field.name, length, *field.maxLength), Hint: "缩短模型 id 后重试", Status: 400}
		}
	}
	return nil
}

func makeView(spec sectionSpec, doc Document) MutationResult {
	value := clone(doc.Base)
	user := clone(doc.User)
	base := clone(doc.Base)
	fields := make([]Field, 0, len(spec.fields))
	for _, fieldSpec := range spec.fields {
		current, ok := user[fieldSpec.name]
		overridden := ok
		if !ok {
			current = doc.Base[fieldSpec.name]
		}
		value[fieldSpec.name] = current
		fields = append(fields, Field{
			Name: fieldSpec.name, Path: []string{fieldSpec.name}, Type: "string",
			Current: current, Default: doc.Base[fieldSpec.name], Overridden: overridden, Writable: true,
			Enum: append([]string(nil), fieldSpec.enum...), MinLength: fieldSpec.minLength, MaxLength: fieldSpec.maxLength,
		})
	}
	if len(user) == 0 {
		user = nil
	}
	return MutationResult{
		Namespace: NamespaceView{
			NS: spec.ns, Schema: schemaFor(spec), Value: value, Base: base, User: user,
			AutoGenerate: true, Applies: "live", Secrets: []string{}, Revision: doc.Revision,
		},
		Section: Section{NS: spec.ns, Title: spec.title, Writable: true, Revision: doc.Revision, Fields: fields, Errors: []Error{}},
	}
}

// schemaFor 使用官方 SettingsSchemaService 能 rehydrate 的 schemastery envelope。
func schemaFor(spec sectionSpec) any {
	if spec.ns == NamespacePermission {
		return map[string]any{
			"uid": 6,
			"refs": map[string]any{
				"1": map[string]any{"type": "const", "value": "deny"},
				"2": map[string]any{"type": "const", "value": "ask"},
				"3": map[string]any{"type": "const", "value": "allow"},
				"4": map[string]any{"type": "const", "value": "auto"},
				"5": map[string]any{"type": "union", "list": []int{1, 2, 3, 4}},
				"6": map[string]any{"type": "object", "dict": map[string]any{FieldDefaultPreset: 5}},
			},
		}
	}
	return map[string]any{"type": "object", "dict": map[string]any{
		FieldDefaultModel: map[string]any{"type": "string", "meta": map[string]any{}},
	}}
}

func clone(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
