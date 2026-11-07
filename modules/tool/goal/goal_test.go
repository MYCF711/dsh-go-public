package goal

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/internal/spi"
)

func ctxWith(id string) context.Context { return spi.WithSession(context.Background(), id) }

func call(t *testing.T, st *store, kind string, ctx context.Context, raw string) (spi.Result, error) {
	t.Helper()
	return (&Tool{st: st, kind: kind}).Exec(ctx, json.RawMessage(raw))
}

// revision 是 CAS：拿旧值来改必须被拒，否则两个调用会互相覆盖。
func TestUpdateRejectsStaleRevision(t *testing.T) {
	st := &store{dir: t.TempDir()}
	ctx := ctxWith("acp-a")
	if _, err := call(t, st, kindCreate, ctx, `{"objective":"把 demo 跑通"}`); err != nil {
		t.Fatal(err)
	}
	// 第一次：revision=1，成功
	if _, err := call(t, st, kindUpdate, ctx, `{"action":"pause","goal_id":"`+goalID(t, st, ctx)+`","revision":1}`); err != nil {
		t.Fatalf("第一次更新应当成功: %v", err)
	}
	// 第二次还用 1：应当被拒
	if _, err := call(t, st, kindUpdate, ctx, `{"action":"resume","goal_id":"`+goalID(t, st, ctx)+`","revision":1}`); err == nil {
		t.Fatal("旧 revision 必须被拒绝")
	}
}

// 一个会话只能有一个目标；重复 create 要给下一步而不是默默覆盖。
func TestCreateTwiceRejected(t *testing.T) {
	st := &store{dir: t.TempDir()}
	ctx := ctxWith("acp-b")
	if _, err := call(t, st, kindCreate, ctx, `{"objective":"第一个"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, st, kindCreate, ctx, `{"objective":"第二个"}`); err == nil {
		t.Fatal("已有目标时再 create 必须被拒")
	}
}

// blocked 必须带理由，否则复盘时没人知道卡在哪。
func TestBlockedRequiresReason(t *testing.T) {
	st := &store{dir: t.TempDir()}
	ctx := ctxWith("acp-c")
	call(t, st, kindCreate, ctx, `{"objective":"x"}`)
	id := goalID(t, st, ctx)
	if _, err := call(t, st, kindUpdate, ctx, `{"action":"blocked","goal_id":"`+id+`","revision":1}`); err == nil {
		t.Fatal("blocked 不带 reason 必须被拒")
	}
	if _, err := call(t, st, kindUpdate, ctx, `{"action":"blocked","goal_id":"`+id+`","revision":1,"blocked_reason":"等 CI"}`); err != nil {
		t.Fatalf("带 reason 应当成功: %v", err)
	}
}

// 没有会话上下文时必须拒绝（不然目标写到谁头上都不知道）。
func TestGoalWithoutSessionRejected(t *testing.T) {
	st := &store{dir: t.TempDir()}
	if _, err := call(t, st, kindCreate, context.Background(), `{"objective":"x"}`); err == nil {
		t.Fatal("没有会话 id 必须拒绝")
	}
}

func goalID(t *testing.T, st *store, ctx context.Context) string {
	t.Helper()
	res, err := call(t, st, kindGet, ctx, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := res.Data.(map[string]any)
	g, ok := m["goal"].(*Goal)
	if !ok || g == nil {
		t.Fatalf("读不到目标: %#v", res.Data)
	}
	return g.ID
}
