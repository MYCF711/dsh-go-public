package settingsform

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type memoryStore struct {
	docs       map[string]Document
	writeErr   error
	writeCalls int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{docs: map[string]Document{
		"permission": {Base: map[string]any{"defaultPreset": "allow"}},
		"model":      {Base: map[string]any{"defaultModel": "base-model"}},
	}}
}

func (s *memoryStore) Read(ns string) (Document, error) {
	doc, ok := s.docs[ns]
	if !ok {
		return Document{}, &Error{Code: "not_found", Message: "unknown namespace", Hint: "use permission or model", Status: 404}
	}
	return Document{Base: clone(doc.Base), User: clone(doc.User), Revision: doc.Revision}, nil
}

func (s *memoryStore) Write(ns string, user map[string]any, expected *int64) (int64, error) {
	s.writeCalls++
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	doc := s.docs[ns]
	if expected != nil && *expected != doc.Revision {
		return 0, RevisionConflict(ns, *expected, doc.Revision)
	}
	doc.Revision++
	doc.User = clone(user)
	s.docs[ns] = doc
	return doc.Revision, nil
}

func TestDescribeGetSetMutateResetAndReopen(t *testing.T) {
	store := newMemoryStore()
	svc := New(store)
	described, err := svc.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if !described.Writable || described.HasDocument || len(described.Namespaces) != 2 || len(described.Sections) != 2 || len(described.Errors) != 0 {
		t.Fatalf("bad describe: %+v", described)
	}
	permission, err := svc.Get("permission")
	if err != nil {
		t.Fatal(err)
	}
	if len(permission.Fields) != 1 || permission.Fields[0].Current != "allow" || permission.Fields[0].Default != "allow" || permission.Fields[0].Overridden {
		t.Fatalf("bad initial permission: %+v", permission)
	}
	if got := permission.Fields[0].Enum; !reflect.DeepEqual(got, []string{"deny", "ask", "allow", "auto"}) {
		t.Fatalf("enum=%v", got)
	}
	model, err := svc.Get("model")
	if err != nil {
		t.Fatal(err)
	}
	if model.Fields[0].MinLength == nil || *model.Fields[0].MinLength != 1 || model.Fields[0].MaxLength == nil || *model.Fields[0].MaxLength != 256 {
		t.Fatalf("model range missing: %+v", model.Fields[0])
	}

	rev := int64(0)
	changed, err := svc.Mutate("permission", []PathOp{{Op: "set", Path: []string{"defaultPreset"}, Value: "deny"}}, &rev)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Namespace.Value["defaultPreset"] != "deny" || changed.Namespace.User["defaultPreset"] != "deny" || changed.Namespace.Revision != 1 || changed.Section.Fields[0].Current != "deny" {
		t.Fatalf("set did not return stored truth: %+v", changed)
	}
	if _, err := svc.Mutate("model", []PathOp{{Op: "set", Path: []string{"defaultModel"}, Value: "model-A"}}, &rev); err != nil {
		t.Fatal(err)
	}

	// A new Service over the same persisted store is the reopen boundary.
	reopened := New(store)
	got, err := reopened.Get("permission")
	if err != nil || got.Fields[0].Current != "deny" || !got.Fields[0].Overridden {
		t.Fatalf("reopen permission=%+v err=%v", got, err)
	}
	reset, err := reopened.Reset("permission", nil, &[]int64{1}[0])
	if err != nil {
		t.Fatal(err)
	}
	if reset.Section.Fields[0].Current != "allow" || reset.Section.Fields[0].Overridden || reset.Namespace.User != nil || reset.Namespace.Revision != 2 {
		t.Fatalf("reset=%+v", reset)
	}
	unset, err := reopened.Mutate("model", []PathOp{{Op: "unset", Path: []string{"defaultModel"}}}, &[]int64{1}[0])
	if err != nil {
		t.Fatal(err)
	}
	if unset.Section.Fields[0].Current != "base-model" || unset.Section.Fields[0].Overridden {
		t.Fatalf("unset=%+v", unset)
	}
}

func TestValidationFailuresAreAtomicAndTyped(t *testing.T) {
	long := strings.Repeat("x", 257)
	tests := []struct {
		name string
		ns   string
		ops  []PathOp
		code string
	}{
		{"bad enum", "permission", []PathOp{{Op: "set", Path: []string{"defaultPreset"}, Value: "block"}}, "invalid_enum"},
		{"out of range", "model", []PathOp{{Op: "set", Path: []string{"defaultModel"}, Value: long}}, "out_of_range"},
		{"empty model", "model", []PathOp{{Op: "set", Path: []string{"defaultModel"}, Value: ""}}, "out_of_range"},
		{"unknown field", "permission", []PathOp{{Op: "set", Path: []string{"other"}, Value: "deny"}}, "unknown_field"},
		{"nested path", "permission", []PathOp{{Op: "set", Path: []string{"defaultPreset", "x"}, Value: "deny"}}, "invalid_path"},
		{"empty path", "permission", []PathOp{{Op: "unset", Path: []string{}}}, "invalid_path"},
		{"wrong type", "permission", []PathOp{{Op: "set", Path: []string{"defaultPreset"}, Value: float64(1)}}, "invalid_type"},
		{"unknown op", "permission", []PathOp{{Op: "replace", Path: []string{"defaultPreset"}, Value: "deny"}}, "bad_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			svc := New(store)
			_, err := svc.Mutate(tc.ns, tc.ops, nil)
			assertCode(t, err, tc.code)
			if store.writeCalls != 0 {
				t.Fatalf("rejected mutation wrote store %d times", store.writeCalls)
			}
			section, getErr := svc.Get(tc.ns)
			if getErr != nil || section.Fields[0].Overridden {
				t.Fatalf("rejected mutation changed value: %+v err=%v", section, getErr)
			}
		})
	}

	svc := New(newMemoryStore())
	_, err := svc.Mutate("missing", nil, nil)
	assertCode(t, err, "not_found")
	_, err = svc.Reset("permission", []string{}, nil)
	assertCode(t, err, "invalid_path")
}

func TestRevisionConflictAndWriteFailureDoNotFakeSuccess(t *testing.T) {
	store := newMemoryStore()
	svc := New(store)
	stale := int64(7)
	_, err := svc.Mutate("permission", []PathOp{{Op: "set", Path: []string{"defaultPreset"}, Value: "deny"}}, &stale)
	assertCode(t, err, "revision_conflict")
	if store.writeCalls != 0 {
		t.Fatal("service should reject a known stale revision before store write")
	}

	store.writeErr = errors.New("disk full")
	_, err = svc.Mutate("permission", []PathOp{{Op: "set", Path: []string{"defaultPreset"}, Value: "deny"}}, nil)
	assertCode(t, err, "internal")
	section, getErr := svc.Get("permission")
	if getErr != nil || section.Fields[0].Current != "allow" || section.Fields[0].Overridden {
		t.Fatalf("failed write appeared successful: %+v err=%v", section, getErr)
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != code || apiErr.Message == "" || apiErr.Hint == "" || apiErr.Status == 0 {
		t.Fatalf("error=%#v, want code %s and four-field metadata", err, code)
	}
}

// Update/Replace 是官方 settings/update|replace 的语义入口：一个是合并、
// 一个是整段替换，提交路径与 Mutate 同一条（revision 栅栏 + 一次原子写）。
func TestUpdateMergesAndReplaceWholesales(t *testing.T) {
	store := newMemoryStore()
	svc := New(store)

	merged, err := svc.Update("permission", map[string]any{"defaultPreset": "deny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Namespace.Revision != 1 || merged.Namespace.User["defaultPreset"] != "deny" {
		t.Fatalf("update 没写进去: %+v", merged.Namespace)
	}
	if merged.Namespace.Value["defaultPreset"] != "deny" || merged.Section.Fields[0].Overridden != true {
		t.Fatalf("update 的结果没反映新值: %+v", merged.Namespace)
	}

	// 合并语义：没提到的键保持原样（这里用空 patch 验证不是整段清空）。
	same, err := svc.Update("permission", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if same.Namespace.User["defaultPreset"] != "deny" || same.Namespace.Revision != 2 {
		t.Fatalf("空 patch 不该清空 user: %+v", same.Namespace)
	}

	replaced, err := svc.Replace("permission", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced.Namespace.User) != 0 || replaced.Namespace.Value["defaultPreset"] != "allow" {
		t.Fatalf("空 section 的 replace 应清空 user 并回落到 base: %+v", replaced.Namespace)
	}
	if replaced.Namespace.Revision != 3 {
		t.Fatalf("清空也是一次真实写入，revision=%d", replaced.Namespace.Revision)
	}
}

func TestUpdateAndReplaceRespectRevisionFence(t *testing.T) {
	store := newMemoryStore()
	svc := New(store)
	stale := int64(7)
	_, err := svc.Update("permission", map[string]any{"defaultPreset": "deny"}, &stale)
	assertCode(t, err, "revision_conflict")
	_, err = svc.Replace("permission", map[string]any{}, &stale)
	assertCode(t, err, "revision_conflict")
	if store.writeCalls != 0 {
		t.Fatalf("已知过期 revision 不该碰存储，实际写了 %d 次", store.writeCalls)
	}

	negative := int64(-1)
	_, err = svc.Update("permission", map[string]any{"defaultPreset": "deny"}, &negative)
	assertCode(t, err, "bad_request")
}

func TestUpdateRejectsInvalidSectionWithoutWriting(t *testing.T) {
	tests := []struct {
		name    string
		ns      string
		section map[string]any
		code    string
	}{
		{"unknown field", "permission", map[string]any{"other": "deny"}, "unknown_field"},
		{"bad enum", "permission", map[string]any{"defaultPreset": "block"}, "invalid_enum"},
		{"wrong type", "permission", map[string]any{"defaultPreset": 1}, "invalid_type"},
		{"null value", "permission", map[string]any{"defaultPreset": nil}, "invalid_type"},
		{"empty model", "model", map[string]any{"defaultModel": "  "}, "out_of_range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemoryStore()
			svc := New(store)
			_, err := svc.Update(tc.ns, tc.section, nil)
			assertCode(t, err, tc.code)
			if store.writeCalls != 0 {
				t.Fatalf("被拒绝的 update 写了存储 %d 次", store.writeCalls)
			}
			_, err = svc.Replace(tc.ns, tc.section, nil)
			assertCode(t, err, tc.code)
			if store.writeCalls != 0 {
				t.Fatalf("被拒绝的 replace 写了存储 %d 次", store.writeCalls)
			}
		})
	}

	svc := New(newMemoryStore())
	_, err := svc.Update("missing", map[string]any{}, nil)
	assertCode(t, err, "not_found")
}

func TestUpdateWriteFailureDoesNotFakeSuccess(t *testing.T) {
	store := newMemoryStore()
	svc := New(store)
	store.writeErr = errors.New("disk full")
	_, err := svc.Update("model", map[string]any{"defaultModel": "next"}, nil)
	assertCode(t, err, "internal")
	section, getErr := svc.Get("model")
	if getErr != nil || section.Fields[0].Current != "base-model" || section.Fields[0].Overridden {
		t.Fatalf("写失败被当成成功: %+v err=%v", section, getErr)
	}
	store.writeErr = nil
	changed, err := svc.Update("model", map[string]any{"defaultModel": "next"}, nil)
	if err != nil || changed.Namespace.User["defaultModel"] != "next" {
		t.Fatalf("恢复后仍写不进去: %+v err=%v", changed.Namespace, err)
	}
}

// Official() 是官方 SettingsNamespaceView 的投影：secrets 必须是对象数组，
// applies 固定 live，value/base/user/revision 原样搬运（不重算）。
func TestOfficialViewMatchesOfficialContract(t *testing.T) {
	doc := Document{
		Base:     map[string]any{"defaultPreset": "allow", "apiKey": ""},
		User:     map[string]any{"defaultPreset": "deny"},
		Revision: 9,
	}
	spec := sectionSpec{ns: "permission", title: "Default permission", fields: []fieldSpec{{name: "defaultPreset", enum: []string{"deny", "ask", "allow", "auto"}}}}
	view := makeView(spec, doc)
	view.Namespace.Secrets = []string{"apiKey"}

	official := view.Namespace.Official()
	if official.NS != "permission" || official.Applies != "live" || !official.AutoGenerate || official.Revision != 9 {
		t.Fatalf("标量字段没搬对: %+v", official)
	}
	if official.Secrets == nil || len(official.Secrets) != 1 {
		t.Fatalf("secrets 必须是数组（空也要是非 nil 空数组）: %+v", official.Secrets)
	}
	if slot := official.Secrets[0]; len(slot.Path) != 1 || slot.Path[0] != "apiKey" || slot.Set {
		t.Fatalf("secret 槽位不对（base 是空串，set 必须是 false）: %+v", slot)
	}
	if official.Value["defaultPreset"] != "deny" || official.User["defaultPreset"] != "deny" || official.Base["defaultPreset"] != "allow" {
		t.Fatalf("value/base/user 没原样搬: %+v", official)
	}

	empty := makeView(spec, Document{Base: map[string]any{"defaultPreset": "allow"}})
	if got := empty.Namespace.Official().Secrets; got == nil || len(got) != 0 {
		t.Fatalf("无 secret 槽位时 secrets 必须是 []（不是 null）: %#v", got)
	}
}
