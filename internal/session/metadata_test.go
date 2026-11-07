package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/codecs/v4"
)

func createMetadataTestSession(t *testing.T, home, id string) string {
	t.Helper()
	logw, err := CreateSession(home, home, id, "standard")
	if err != nil {
		t.Fatal(err)
	}
	if err := logw.Append("system/message", SystemMessageData(1, 1, "system")); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}
	return logw.Path
}

func TestMetadataIndexCommitFailureRecoversFromV4Intent(t *testing.T) {
	for _, kind := range []string{"session/title", "session/color"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := createMetadataTestSession(t, home, "recover-"+strings.ReplaceAll(kind, "/", "-"))
			indexPath, _, err := metadataPaths(path, kind)
			if err != nil {
				t.Fatal(err)
			}
			// 让最终 os.Rename(tmp,index) 必然失败，同时允许 intent 正常落盘。
			if err := os.Mkdir(indexPath, 0o755); err != nil {
				t.Fatal(err)
			}
			logw, err := OpenSessionAt(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "session/title":
				err = AppendSessionTitle(logw, "  recovered title  ")
			case "session/color":
				err = AppendSessionColor(logw, "recovered-color")
			}
			if err == nil {
				t.Fatal("index commit unexpectedly succeeded")
			}
			if closeErr := logw.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			switch kind {
			case "session/title":
				title, source, present, readErr := readExplicitTitle(path)
				if readErr != nil || !present || title == nil || *title != "  recovered title  " || source != "user" {
					t.Fatalf("v4 title recovery title=%v source=%q present=%v err=%v", title, source, present, readErr)
				}
			case "session/color":
				color, readErr := ReadSessionColor(path)
				if readErr != nil || color != "recovered-color" {
					t.Fatalf("v4 color recovery color=%q err=%v", color, readErr)
				}
			}
		})
	}
}

func TestNextMetadataMutationRepairsRecoveredIndexBeforeOverwritingIntent(t *testing.T) {
	home := t.TempDir()
	path := createMetadataTestSession(t, home, "repair-before-next")
	indexPath := filepath.Join(filepath.Dir(path), "session-title.json")
	if err := os.Mkdir(indexPath, 0o755); err != nil {
		t.Fatal(err)
	}
	logw, err := OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendSessionTitle(logw, "first"); err == nil {
		t.Fatal("first index commit unexpectedly succeeded")
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	logw, err = OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendSessionTitle(logw, "second"); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err != nil {
		t.Fatal(err)
	}
	title, _, present, err := readExplicitTitle(path)
	if err != nil || !present || title == nil || *title != "second" {
		t.Fatalf("next mutation title=%v present=%v err=%v", title, present, err)
	}
}

func TestOpenSessionAtClaimsWriterBeforeReadingSnapshot(t *testing.T) {
	home := t.TempDir()
	path := createMetadataTestSession(t, home, "lock-before-read")
	if err := os.WriteFile(path, []byte("not a v4 log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := claimWriter(path); err != nil {
		t.Fatal(err)
	}
	defer releaseWriter(path)
	_, err := OpenSessionAt(path)
	if err == nil || !strings.Contains(err.Error(), "另一个进程") {
		t.Fatalf("OpenSessionAt read before lock, error=%v", err)
	}
}

func TestOpenSessionClaimsWriterBeforeReadingSnapshot(t *testing.T) {
	home := t.TempDir()
	path := createMetadataTestSession(t, home, "open-lock-before-read")
	if err := os.WriteFile(path, []byte("not a v4 log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := claimWriter(path); err != nil {
		t.Fatal(err)
	}
	defer releaseWriter(path)
	_, err := OpenSession(home, home, "open-lock-before-read")
	if err == nil || !strings.Contains(err.Error(), "另一个进程") {
		t.Fatalf("OpenSession read before lock, error=%v", err)
	}
}

func TestOpenSessionAtConcurrentWritersKeepV4SeqContiguous(t *testing.T) {
	home := t.TempDir()
	path := createMetadataTestSession(t, home, "concurrent-writers")
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var logw *Log
			var err error
			deadline := time.Now().Add(10 * time.Second)
			for {
				logw, err = OpenSessionAt(path)
				if err == nil {
					break
				}
				// Windows 在旧锁句柄关闭、目录项删除与下一次 O_EXCL 创建
				// 交界也可能瞬时报 Access is denied，和显式写锁同属竞争。
				if !strings.Contains(err.Error(), "另一个进程") && !strings.Contains(err.Error(), "Access is denied") {
					errs <- err
					return
				}
				if time.Now().After(deadline) {
					errs <- fmt.Errorf("writer %d did not acquire session lock: %w", i, err)
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			if err := AppendSessionColor(logw, fmt.Sprintf("color-%02d", i)); err != nil {
				_ = logw.Close()
				errs <- err
				return
			}
			if err := logw.Close(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	log, err := v4.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Events) != writers+1 {
		t.Fatalf("events=%d want=%d", len(log.Events), writers+1)
	}
	for i, event := range log.Events {
		if event.Seq != int64(i) {
			t.Fatalf("event[%d] seq=%d, duplicate/stale snapshot detected", i, event.Seq)
		}
	}
}

func TestLogCloseReportsLockReleaseFailure(t *testing.T) {
	home := t.TempDir()
	path := createMetadataTestSession(t, home, "lock-close-error")
	logw, err := OpenSessionAt(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "child"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := logw.Close(); err == nil || !strings.Contains(err.Error(), "释放会话写锁") {
		t.Fatalf("Close lock release error=%v", err)
	}
}
