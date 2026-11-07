package teamtask

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/internal/spi"
)

// execTool 用同一个数据目录构造一次工具调用：kind 决定走哪个 action。
func execTool(t *testing.T, dir, kind, session string, args map[string]any) (spi.Result, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	tl := &Tool{b: &board{dir: dir}, kind: kind}
	return tl.Exec(spi.WithSession(context.Background(), session), raw)
}

// createTask 建一条任务并返回它的 id。
func createTask(t *testing.T, dir, session, subject string) string {
	t.Helper()
	res, err := execTool(t, dir, kindCreate, session, map[string]any{
		"subject":     subject,
		"description": "for test: " + subject,
	})
	if err != nil {
		t.Fatalf("create %q: %v", subject, err)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("create Data type = %T, want map[string]any", res.Data)
	}
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("create returned no id: %#v", data)
	}
	return id
}

// claimTask 让 session 认领任务。不传 owner 键：实现按会话 id 署名，
// 而 delete 的"是不是本人"判定看的正是这个身份（ownerOf -> SessionFrom）。
func claimTask(t *testing.T, dir, session, id string) {
	t.Helper()
	if _, err := execTool(t, dir, kindUpdate, session, map[string]any{
		"task_id": id,
		"action":  "claim",
	}); err != nil {
		t.Fatalf("claim %s as %s: %v", id, session, err)
	}
}

func TestDeleteRefusesOtherOwnersTask(t *testing.T) {
	dir := t.TempDir()
	id := createTask(t, dir, "session-alice", "task owned by session-alice")
	claimTask(t, dir, "session-alice", id)

	// bob 是另一个会话，没带 force，应当被拒。
	res, err := execTool(t, dir, kindUpdate, "session-bob", map[string]any{
		"task_id": id,
		"action":  "delete",
	})
	if err == nil {
		t.Fatalf("delete by other session succeeded, want refusal")
	}
	if !strings.Contains(res.Hint, "alice") {
		t.Errorf("Hint = %q, want it to mention owner %q", res.Hint, "alice")
	}
	if !strings.Contains(res.Hint, "force") {
		t.Errorf("Hint = %q, want it to mention %q", res.Hint, "force")
	}
}

func TestDeleteForceRemovesClaimedTask(t *testing.T) {
	dir := t.TempDir()
	id := createTask(t, dir, "session-alice", "task force-deleted by session-bob")
	claimTask(t, dir, "session-alice", id)

	res, err := execTool(t, dir, kindUpdate, "session-bob", map[string]any{
		"task_id": id,
		"action":  "delete",
		"force":   true,
	})
	if err != nil {
		t.Fatalf("forced delete by other session: %v (hint: %s)", err, res.Hint)
	}

	// 任务真的没了：再 get 应当报错。
	gres, gerr := execTool(t, dir, kindGet, "session-bob", map[string]any{"task_id": id})
	if gerr == nil {
		t.Fatalf("get after delete succeeded, task still there: %#v (hint: %s)", gres.Data, gres.Hint)
	}
}

func TestDeleteOwnTaskAllowed(t *testing.T) {
	dir := t.TempDir()
	id := createTask(t, dir, "session-alice", "task session-alice deletes herself")
	claimTask(t, dir, "session-alice", id)

	res, err := execTool(t, dir, kindUpdate, "session-alice", map[string]any{
		"task_id": id,
		"action":  "delete",
	})
	if err != nil {
		t.Fatalf("owner delete without force: %v (hint: %s)", err, res.Hint)
	}
}

func TestDeleteArchivesTask(t *testing.T) {
	dir := t.TempDir()
	id := createTask(t, dir, "session-alice", "task to be archived")

	if _, err := execTool(t, dir, kindUpdate, "session-alice", map[string]any{
		"task_id": id,
		"action":  "delete",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	path := filepath.Join(dir, "team-tasks.deleted.jsonl")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive %s: %v", path, err)
	}

	var lines []string
	for _, ln := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("archive %s is empty", path)
	}

	type archived struct {
		DeletedAt json.RawMessage `json:"deleted_at"`
		Task      json.RawMessage `json:"task"`
	}

	found := false
	for i, ln := range lines {
		var a archived
		if err := json.Unmarshal([]byte(ln), &a); err != nil {
			t.Fatalf("line %d is not valid JSON: %v (%s)", i, err, ln)
		}
		if len(a.DeletedAt) == 0 || string(a.DeletedAt) == "null" {
			t.Errorf("line %d: missing deleted_at: %s", i, ln)
			continue
		}
		if len(a.Task) == 0 || string(a.Task) == "null" {
			t.Errorf("line %d: missing task: %s", i, ln)
			continue
		}
		var task map[string]any
		if err := json.Unmarshal(a.Task, &task); err != nil {
			t.Fatalf("line %d: task is not a JSON object: %v (%s)", i, err, string(a.Task))
		}
		gotID := ""
		if v, ok := task["task_id"].(string); ok {
			gotID = v
		} else if v, ok := task["id"].(string); ok {
			gotID = v
		}
		if gotID == id {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("archive has no entry for deleted task %q; content:\n%s", id, string(body))
	}
}
