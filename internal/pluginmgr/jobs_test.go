package pluginmgr

// 安装作业（requestId / wait / cancel）的确定性测试。
//
// 判据是"真实事务"：取消必须在复制/换目录/提交状态之间真的停下，并把已复制的
// 版本目录回滚掉；`too-late` 只在状态已经落盘之后，绝不允许把"没停下"报成
// `cancelled`。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blockAt 把管理器卡在某个事务阶段，返回"已到达"与"放行"两个信号。
func blockAt(t *testing.T, m *Manager, phase string) (reached <-chan struct{}, release func()) {
	t.Helper()
	reachedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var once bool
	m.afterPhase = func(got string) error {
		if got == phase && !once {
			once = true
			close(reachedCh)
			<-releaseCh
		}
		return nil
	}
	t.Cleanup(func() {
		select {
		case <-releaseCh:
		default:
			close(releaseCh)
		}
		m.afterPhase = nil
	})
	return reachedCh, func() {
		select {
		case <-releaseCh:
		default:
			close(releaseCh)
		}
	}
}

func TestInstallJobCancelsDuringCopyAndRollsBack(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	source := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := blockAt(t, m, "staged")

	job, err := m.StartInstall(source, InstallOptions{RequestID: "req-cancel", Enable: true})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	<-reached

	// requestCancel 只置位；CancelInstall 会等到作业真的收工。
	status, handle := m.requestCancel("req-cancel")
	if status != "cancelling" || handle == nil {
		t.Fatalf("requestCancel 状态=%q handle=%v", status, handle)
	}
	release()

	outcome := job.Wait()
	if outcome.Application != "cancelled" || outcome.Changed {
		t.Fatalf("取消后的作业结果不对：%+v", outcome)
	}
	if _, err := os.Stat(m.packagePath("acme.audit", "1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("取消后版本目录还在：%v", err)
	}
	plugins, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 0 {
		t.Fatalf("取消后状态里出现插件：%+v", plugins)
	}
	// 事务目录也必须收干净，否则重启 reconcile 会把它当残缺事务。
	entries, err := os.ReadDir(filepath.Join(root, "transactions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("取消后残留事务 %d 个", len(entries))
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("取消后重开失败（事务没收干净）：%v", err)
	}
	if list, err := reopened.List(); err != nil || len(list) != 0 {
		t.Fatalf("重开后状态不干净：%v %+v", err, list)
	}
	// 作业收工后同名 id 不再在飞：这些都是官方的 null/not-running 语义。
	if _, ok := m.WaitInstall("req-cancel"); ok {
		t.Fatal("已完成的作业不该仍可 wait")
	}
	if status := m.CancelInstall("req-cancel"); status != "not-running" {
		t.Fatalf("已完成作业的 cancel 状态=%q，期望 not-running", status)
	}
}

func TestInstallJobWaitSeesRealResult(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	source := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := blockAt(t, m, "installed")

	job, err := m.StartInstall(source, InstallOptions{RequestID: "req-wait", Enable: true})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	<-reached

	done := make(chan InstallOutcome, 1)
	go func() {
		outcome, ok := m.WaitInstall("req-wait")
		if !ok {
			done <- InstallOutcome{Application: "missing"}
			return
		}
		done <- outcome
	}()
	select {
	case outcome := <-done:
		t.Fatalf("wait 在作业还在飞时就返回了：%+v", outcome)
	case <-time.After(50 * time.Millisecond):
	}
	release()

	outcome := <-done
	if outcome.Application != "applied" || !outcome.Changed || outcome.Info.ID != "acme.audit" {
		t.Fatalf("wait 结果不对：%+v", outcome)
	}
	if outcome.Enabled == nil || !*outcome.Enabled {
		t.Fatalf("enabled 选项没落到真实状态：%+v", outcome)
	}
	if job.Wait().Application != "applied" {
		t.Fatalf("job.Wait 与 WaitInstall 不一致：%+v", job.Wait())
	}
	plugins, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || !plugins[0].Enabled || plugins[0].Version != "1.0.0" {
		t.Fatalf("启用位没持久化：%+v", plugins)
	}
}

func TestInstallJobTooLateAfterCommit(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	source := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := blockAt(t, m, "committing")

	job, err := m.StartInstall(source, InstallOptions{RequestID: "req-late"})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	<-reached

	status, _ := m.requestCancel("req-late")
	if status != "too-late" {
		t.Fatalf("提交阶段 cancel 状态=%q，期望 too-late", status)
	}
	release()
	outcome := job.Wait()
	if outcome.Application != "applied" || !outcome.Changed {
		t.Fatalf("too-late 之后的状态不对：%+v", outcome)
	}
	if plugins, err := m.List(); err != nil || len(plugins) != 1 {
		t.Fatalf("too-late 之后的真实状态不对：%v %+v", err, plugins)
	}
}

func TestInstallJobRejectsDuplicateRequestID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	source := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := blockAt(t, m, "staged")
	if _, err := m.StartInstall(source, InstallOptions{RequestID: "req-dup"}); err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	<-reached
	_, err = m.StartInstall(source, InstallOptions{RequestID: "req-dup"})
	if err == nil {
		t.Fatal("重复 requestId 被接受了：会把两次安装的进度/取消混在一起")
	}
	if !strings.Contains(err.Error(), "req-dup") {
		t.Fatalf("重复 requestId 的错误没点名：%v", err)
	}
	release()
	m.requestCancel("req-dup")
	if _, ok := m.WaitInstall("req-dup"); !ok {
		t.Fatal("首个作业应当仍在（被取消后收工）")
	}
}

func TestInstallJobFailureIsReportedAsFailed(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	source := writeTestPackage(t, filepath.Join(home, "v1"), "acme.audit", "1.0.0")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	m.afterPhase = func(phase string) error {
		if phase == "committing" {
			return os.ErrPermission
		}
		return nil
	}
	job, err := m.StartInstall(source, InstallOptions{RequestID: "req-fail"})
	if err != nil {
		t.Fatalf("StartInstall: %v", err)
	}
	outcome := job.Wait()
	if outcome.Application != "failed" || outcome.Changed {
		t.Fatalf("失败作业的结果不对：%+v", outcome)
	}
	if outcome.Failure == nil || outcome.Failure.Code == "" {
		t.Fatalf("失败作业必须带四字段失败原因：%+v", outcome)
	}
	if plugins, err := m.List(); err != nil || len(plugins) != 0 {
		t.Fatalf("失败后状态被改了：%v %+v", err, plugins)
	}
}

// 卸载/启停没有 requestId（官方契约里也没有），但它们的失败必须是四字段错误，
// 不能被包装成一次"成功"。
func TestEnableAndRemoveReportManagementFailures(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "manager")
	m, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Enable("nope.gone"); err == nil {
		t.Fatal("启用不存在的包成功了")
	} else if apiErr, ok := err.(*Error); !ok || apiErr.Code != "not_found" {
		t.Fatalf("启用不存在的包的错误不对：%v", err)
	}
	if err := m.Uninstall("nope.gone"); err == nil {
		t.Fatal("卸载不存在的包成功了")
	}
}
