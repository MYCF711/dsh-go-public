package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dshgo/internal/session"
	"dshgo/internal/transport"
)

// sessionMetadataService 串行化同一生产宿主内的 title/color mutation，并把
// journal/WS commit 包含在同一临界区。否则 A 写盘后暂停、B 写盘并先发布时，
// A 的迟到旧帧会让客户端看到与 durable store 相反的最终值。
type sessionMetadataService struct {
	home string
	mu   sync.Mutex
}

func newSessionMetadataService(home string) *sessionMetadataService {
	return &sessionMetadataService{home: home}
}

func (s *sessionMetadataService) Rename(sessionID, title string, commit func() error) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("title 不能为空白")
	}
	return s.mutate(sessionID, func(logw *session.Log) error {
		return session.AppendSessionTitle(logw, title)
	}, commit)
}

func (s *sessionMetadataService) SetColor(sessionID, color string, commit func() error) error {
	return s.mutate(sessionID, func(logw *session.Log) error {
		return session.AppendSessionColor(logw, color)
	}, commit)
}

func (s *sessionMetadataService) WorkspaceInit(cwd string) (transport.WorkspaceInitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return serveWorkspaceInit(cwd)
}

func (s *sessionMetadataService) Color(sessionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := newSessionSource(s.home, "").resolve(sessionID)
	if err != nil {
		return "", err
	}
	color, err := session.ReadSessionColor(path)
	if err != nil {
		return "", fmt.Errorf("读取会话 %s 的 color 索引: %w", sessionID, err)
	}
	return color, nil
}

func (s *sessionMetadataService) mutate(sessionID string, appendMetadata func(*session.Log) error, commit func() error) error {
	if commit == nil {
		return fmt.Errorf("metadata change journal 未接线")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := newSessionSource(s.home, "").resolve(sessionID)
	if err != nil {
		return err
	}
	logw, err := session.OpenSessionAt(path)
	if err != nil {
		return fmt.Errorf("打开会话 %s 的 metadata 写入: %w", sessionID, err)
	}
	if err := appendMetadata(logw); err != nil {
		if closeErr := logw.Close(); closeErr != nil {
			return errors.Join(err, fmt.Errorf("关闭 metadata 写者并释放锁: %w", closeErr))
		}
		return err
	}
	if err := logw.Close(); err != nil {
		return fmt.Errorf("落盘会话 %s 的 metadata: %w", sessionID, err)
	}
	// 必须在同一锁内等 journal 持久化并发布；commit 失败则不返回成功 ACK。
	// 失败 mutation 不排队延迟补播：helper 已收到失败，后续只允许成功 mutation
	// 发布当时的最新值，不能在下次操作中复活这次未 ACK 的旧 change。
	if err := commit(); err != nil {
		return fmt.Errorf("%w: %v", transport.ErrStatePersistence, err)
	}
	return nil
}

const workspaceInitTemplate = "# Repository Guidelines\n\n## Project Structure\n\n## Build And Test\n"

// serveWorkspaceInit 当前处理根目录 AGENTS.md。保留逐项报告结构，后续增加模板
// 目标时仍沿用同一 no-overwrite/部分失败语义，不把 Transport 错误伪造成文件报告。
func serveWorkspaceInit(cwd string) (transport.WorkspaceInitResult, error) {
	if strings.TrimSpace(cwd) == "" {
		return transport.WorkspaceInitResult{}, fmt.Errorf("cwd 为空")
	}
	abs, err := filepath.Abs(filepath.FromSlash(cwd))
	if err != nil {
		return transport.WorkspaceInitResult{}, fmt.Errorf("解析 cwd: %w", err)
	}
	path := filepath.Join(abs, "AGENTS.md")
	report := transport.WorkspaceInitResult{Files: []transport.WorkspaceInitFile{{Path: path, Status: "failed"}}}
	fail := func(err error) (transport.WorkspaceInitResult, error) {
		report.Files[0].Error = err.Error()
		return report, nil
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fail(fmt.Errorf("创建 workspace 目录失败: %w", err))
	}

	// 最终路径只在模板已经完整 Write+Sync+Close 后出现。os.Link 在同目录
	// 原子发布且不覆盖既有目标；失败清理只碰本次唯一临时文件，绝不按
	// AGENTS.md 路径盲删其他 actor 已替换的内容。
	tmp, err := os.CreateTemp(abs, ".agents-init-*.tmp")
	if err != nil {
		return fail(fmt.Errorf("创建 AGENTS.md 临时文件失败: %w", err))
	}
	tmpPath := tmp.Name()
	cleanup := func() error {
		err := os.Remove(tmpPath)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	n, writeErr := tmp.Write([]byte(workspaceInitTemplate))
	if writeErr == nil && n != len(workspaceInitTemplate) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		cleanupErr := cleanup()
		switch {
		case writeErr != nil && cleanupErr != nil:
			return fail(fmt.Errorf("写 AGENTS.md 临时文件失败: %v；清理失败: %v", writeErr, cleanupErr))
		case writeErr != nil:
			return fail(fmt.Errorf("写 AGENTS.md 临时文件失败: %w", writeErr))
		case cleanupErr != nil:
			return fail(fmt.Errorf("关闭 AGENTS.md 临时文件失败: %v；清理失败: %v", closeErr, cleanupErr))
		default:
			return fail(fmt.Errorf("关闭 AGENTS.md 临时文件失败: %w", closeErr))
		}
	}

	linkErr := os.Link(tmpPath, path)
	if errors.Is(linkErr, os.ErrExist) {
		info, statErr := os.Stat(path)
		switch {
		case statErr != nil:
			if cleanupErr := cleanup(); cleanupErr != nil {
				return fail(fmt.Errorf("检查已有 AGENTS.md 失败: %v；清理临时文件失败: %v", statErr, cleanupErr))
			}
			return fail(fmt.Errorf("检查已有 AGENTS.md 失败: %w", statErr))
		case !info.Mode().IsRegular():
			if cleanupErr := cleanup(); cleanupErr != nil {
				return fail(fmt.Errorf("AGENTS.md 已存在但不是普通文件；清理临时文件失败: %v", cleanupErr))
			}
			return fail(fmt.Errorf("AGENTS.md 已存在但不是普通文件"))
		default:
			if cleanupErr := cleanup(); cleanupErr != nil {
				fmt.Fprintf(os.Stderr, "workspace.init 临时文件清理失败（已有用户文件未改）: %v\n", cleanupErr)
			}
			report.Files[0].Status = "unchanged"
			return report, nil
		}
	}
	if linkErr != nil {
		cleanupErr := cleanup()
		if cleanupErr != nil {
			return fail(fmt.Errorf("不覆盖发布 AGENTS.md 失败: %v；清理临时文件失败: %v", linkErr, cleanupErr))
		}
		return fail(fmt.Errorf("不覆盖发布 AGENTS.md 失败: %w", linkErr))
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "workspace.init 临时文件清理失败（AGENTS.md 已完整创建）: %v\n", cleanupErr)
	}
	report.Files[0].Status = "created"
	return report, nil
}
