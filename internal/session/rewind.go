package session

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"dshgo/codecs/v4"
)

// RewindFilesPreview 描述从当前工作区恢复到 anchor 时会发生的文件变化。
// Insertions/Deletions 的方向均为 current -> checkpoint。
type RewindFilesPreview struct {
	FilesChanged []string `json:"filesChanged"`
	Insertions   int      `json:"insertions"`
	Deletions    int      `json:"deletions"`
}

const (
	rewindFormatVersion   = 1
	rewindMaxFiles        = 4096
	rewindMaxFileBytes    = 8 << 20
	rewindMaxTotalBytes   = 64 << 20
	rewindMaxManifestSize = 32 << 20
	rewindMaxDiffCells    = 1 << 20
)

type rewindCheckpoint struct {
	Version          int                    `json:"version"`
	SessionID        string                 `json:"sessionId"`
	AnchorSeq        int64                  `json:"anchorSeq"`
	Root             string                 `json:"root"`
	Complete         bool                   `json:"complete"`
	IncompleteReason string                 `json:"incompleteReason,omitempty"`
	Files            []rewindCheckpointFile `json:"files"`
}

type rewindCheckpointFile struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Blob   string `json:"blob"`
}

type rewindFileState struct {
	path string
	mode os.FileMode
	data []byte
}

type rewindPlan struct {
	root    string
	target  map[string]rewindFileState
	current map[string]rewindFileState
	changed []string
	preview RewindFilesPreview
}

// CaptureRewindCheckpoint 把 cwd 当前的 regular files 持久化到 home。
// 目录先以 incomplete.json 发布，所有 blob 与 manifest 校验信息写完才发布完整
// manifest；崩溃、超限或读取失败因此不会被后续进程误认为空 checkpoint。
func CaptureRewindCheckpoint(home, sessionID string, anchorSeq int64, cwd string) (retErr error) {
	dir, err := rewindCheckpointDir(home, sessionID, anchorSeq)
	if err != nil {
		return err
	}
	root, err := rewindValidateRoot(home, cwd)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("rewind checkpoint already exists for session %q anchor %d", sessionID, anchorSeq)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect rewind checkpoint: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("create rewind checkpoint parent: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create rewind checkpoint: %w", err)
	}

	cp := rewindCheckpoint{
		Version:          rewindFormatVersion,
		SessionID:        sessionID,
		AnchorSeq:        anchorSeq,
		Root:             root,
		Complete:         false,
		IncompleteReason: "capture has not completed",
		Files:            make([]rewindCheckpointFile, 0),
	}
	if err := rewindWriteJSON(filepath.Join(dir, "incomplete.json"), cp); err != nil {
		return fmt.Errorf("mark rewind checkpoint incomplete: %w", err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		cp.Complete = false
		cp.IncompleteReason = retErr.Error()
		if err := rewindWriteJSON(filepath.Join(dir, "incomplete.json"), cp); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("update incomplete checkpoint marker: %w", err))
		}
	}()

	files, err := rewindScanFiles(home, root)
	if err != nil {
		return fmt.Errorf("capture rewind files: %w", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return fmt.Errorf("create rewind blob directory: %w", err)
	}
	for i, file := range files {
		blob := fmt.Sprintf("blobs/%08d", i)
		if err := rewindWriteNewFile(filepath.Join(dir, filepath.FromSlash(blob)), file.data, 0o600); err != nil {
			return fmt.Errorf("write rewind blob for %s: %w", file.path, err)
		}
		sum := sha256.Sum256(file.data)
		cp.Files = append(cp.Files, rewindCheckpointFile{
			Path:   file.path,
			Mode:   uint32(file.mode.Perm()),
			Size:   int64(len(file.data)),
			SHA256: hex.EncodeToString(sum[:]),
			Blob:   blob,
		})
	}
	cp.Complete = true
	cp.IncompleteReason = ""
	if err := rewindPublishManifest(dir, cp); err != nil {
		return fmt.Errorf("publish rewind checkpoint: %w", err)
	}
	if err := os.Remove(filepath.Join(dir, "incomplete.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove incomplete checkpoint marker: %w", err)
	}
	return nil
}

// PreviewRewindFiles 只读取并完整校验 checkpoint 和当前文件，不写工作区。
func PreviewRewindFiles(home, sessionID string, anchorSeq int64, cwd string) (RewindFilesPreview, error) {
	plan, err := rewindBuildPlan(home, sessionID, anchorSeq, cwd)
	if err != nil {
		return RewindFilesPreview{}, err
	}
	return plan.preview, nil
}

// RestoreRewindFiles 恢复 checkpoint。所有被替换/删除的当前文件先移动进
// cwd 内的事务目录，失败时按 journal 逆序回滚；成功后才删除备份。
func RestoreRewindFiles(home, sessionID string, anchorSeq int64, cwd string) (RewindFilesPreview, error) {
	return restoreRewindFiles(home, sessionID, anchorSeq, cwd, nil)
}

// beforeInstall 是恢复失败测试的确定性注入口；生产调用恒为 nil。
func restoreRewindFiles(home, sessionID string, anchorSeq int64, cwd string, beforeInstall func(string) error) (RewindFilesPreview, error) {
	plan, err := rewindBuildPlan(home, sessionID, anchorSeq, cwd)
	if err != nil {
		return RewindFilesPreview{}, err
	}
	if len(plan.changed) == 0 {
		return plan.preview, nil
	}
	return rewindApplyPlan(plan, beforeInstall)
}

func rewindCheckpointDir(home, sessionID string, anchorSeq int64) (string, error) {
	if home == "" {
		return "", fmt.Errorf("rewind home is empty")
	}
	if anchorSeq < 0 {
		return "", fmt.Errorf("rewind anchor seq must be non-negative")
	}
	seg, err := v4.EncodeSegment(sessionID)
	if err != nil {
		return "", fmt.Errorf("encode rewind session id: %w", err)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve rewind home: %w", err)
	}
	return filepath.Join(absHome, "rewind-checkpoints", seg, strconv.FormatInt(anchorSeq, 10)), nil
}

func rewindValidateRoot(home, cwd string) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("rewind cwd is empty")
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve rewind cwd: %w", err)
	}
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("inspect rewind cwd: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("rewind cwd must be a real directory, not a symlink or file: %s", root)
	}
	absHome, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve rewind home: %w", err)
	}
	for _, reserved := range []string{
		filepath.Join(absHome, "rewind-checkpoints"),
		filepath.Join(absHome, "sessions"),
	} {
		if rewindPathWithin(root, reserved) {
			return "", fmt.Errorf("rewind cwd cannot be DSH internal state: %s", root)
		}
	}
	return root, nil
}

func rewindPathWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func rewindSamePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func rewindWriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func rewindWriteNewFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func rewindPublishManifest(dir string, cp rewindCheckpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "manifest-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

func rewindReadLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, limit+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d-byte limit", limit)
	}
	return data, nil
}

func rewindReadRegular(path string, walked os.FileInfo) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(walked, opened) {
		_ = f.Close()
		return nil, fmt.Errorf("file changed type or identity during scan")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, rewindMaxFileBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > rewindMaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d-byte capture limit", rewindMaxFileBytes)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(walked, after) || int64(len(data)) != walked.Size() {
		return nil, fmt.Errorf("file changed during scan")
	}
	return data, nil
}

func rewindScanFiles(home, root string) ([]rewindFileState, error) {
	absHome, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	reserved := []string{
		filepath.Join(absHome, "rewind-checkpoints"),
		filepath.Join(absHome, "sessions"),
	}
	files := make([]rewindFileState, 0)
	total := 0
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		skip := entry.Name() == ".git" || entry.Name() == "node_modules" ||
			(filepath.Dir(path) == root && entry.IsDir() && strings.HasPrefix(entry.Name(), ".dsh-rewind-"))
		for _, base := range reserved {
			if rewindPathWithin(path, base) {
				skip = true
				break
			}
		}
		if skip {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			// WalkDir 不跟随目录 symlink；文件 symlink 也只跳过，不打开目标。
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if len(files) >= rewindMaxFiles {
			return fmt.Errorf("capture exceeds %d-file limit", rewindMaxFiles)
		}
		if info.Size() < 0 || info.Size() > rewindMaxFileBytes {
			return fmt.Errorf("%s exceeds %d-byte capture limit", filepath.ToSlash(rel), rewindMaxFileBytes)
		}
		if info.Size() > int64(rewindMaxTotalBytes-total) {
			return fmt.Errorf("capture exceeds %d-byte total limit", rewindMaxTotalBytes)
		}
		data, err := rewindReadRegular(path, info)
		if err != nil {
			return fmt.Errorf("read %s: %w", filepath.ToSlash(rel), err)
		}
		total += len(data)
		files = append(files, rewindFileState{
			path: filepath.ToSlash(rel),
			mode: info.Mode().Perm(),
			data: data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, nil
}

func rewindValidateRelativePath(rel string) error {
	if rel == "" || rel == "." || strings.ContainsRune(rel, 0) || strings.Contains(rel, `\`) {
		return fmt.Errorf("invalid checkpoint relative path %q", rel)
	}
	if pathpkg.IsAbs(rel) || pathpkg.Clean(rel) != rel {
		return fmt.Errorf("non-canonical checkpoint path %q", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" || part == "node_modules" {
			return fmt.Errorf("unsafe checkpoint path %q", rel)
		}
	}
	native := filepath.FromSlash(rel)
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" {
		return fmt.Errorf("absolute checkpoint path %q", rel)
	}
	return nil
}

func rewindSecureJoin(root, rel string) (string, error) {
	if err := rewindValidateRelativePath(rel); err != nil {
		return "", err
	}
	joined := filepath.Join(root, filepath.FromSlash(rel))
	back, err := filepath.Rel(root, joined)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("checkpoint path escapes cwd: %q", rel)
	}
	return joined, nil
}

func rewindLoadCheckpoint(home, sessionID string, anchorSeq int64) (rewindCheckpoint, map[string]rewindFileState, error) {
	var cp rewindCheckpoint
	dir, err := rewindCheckpointDir(home, sessionID, anchorSeq)
	if err != nil {
		return cp, nil, err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := rewindReadLimited(manifestPath, rewindMaxManifestSize)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if marker, markerErr := rewindReadLimited(filepath.Join(dir, "incomplete.json"), rewindMaxManifestSize); markerErr == nil {
				var incomplete rewindCheckpoint
				if json.Unmarshal(marker, &incomplete) == nil && incomplete.IncompleteReason != "" {
					return cp, nil, fmt.Errorf("rewind checkpoint incomplete: %s", incomplete.IncompleteReason)
				}
				return cp, nil, fmt.Errorf("rewind checkpoint incomplete")
			}
		}
		return cp, nil, fmt.Errorf("read rewind checkpoint manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return cp, nil, fmt.Errorf("decode rewind checkpoint manifest: %w", err)
	}
	if cp.Version != rewindFormatVersion || cp.SessionID != sessionID || cp.AnchorSeq != anchorSeq {
		return cp, nil, fmt.Errorf("rewind checkpoint identity or version mismatch")
	}
	if !cp.Complete {
		return cp, nil, fmt.Errorf("rewind checkpoint incomplete: %s", cp.IncompleteReason)
	}
	if cp.Root == "" || cp.Files == nil || len(cp.Files) > rewindMaxFiles {
		return cp, nil, fmt.Errorf("rewind checkpoint manifest is incomplete or exceeds limits")
	}
	files := make(map[string]rewindFileState, len(cp.Files))
	total := 0
	for i, entry := range cp.Files {
		if err := rewindValidateRelativePath(entry.Path); err != nil {
			return cp, nil, err
		}
		if i > 0 && cp.Files[i-1].Path >= entry.Path {
			return cp, nil, fmt.Errorf("checkpoint paths are duplicated or unsorted at %q", entry.Path)
		}
		if entry.Size < 0 || entry.Size > rewindMaxFileBytes || entry.Size > int64(rewindMaxTotalBytes-total) {
			return cp, nil, fmt.Errorf("checkpoint size limits exceeded at %q", entry.Path)
		}
		if entry.Mode&^uint32(0o777) != 0 {
			return cp, nil, fmt.Errorf("checkpoint mode is invalid at %q", entry.Path)
		}
		wantBlob := fmt.Sprintf("blobs/%08d", i)
		if entry.Blob != wantBlob {
			return cp, nil, fmt.Errorf("checkpoint blob path is invalid at %q", entry.Path)
		}
		wantHash, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(wantHash) != sha256.Size {
			return cp, nil, fmt.Errorf("checkpoint hash is invalid at %q", entry.Path)
		}
		data, err := rewindReadLimited(filepath.Join(dir, filepath.FromSlash(entry.Blob)), entry.Size)
		if err != nil {
			return cp, nil, fmt.Errorf("read checkpoint blob for %q: %w", entry.Path, err)
		}
		if int64(len(data)) != entry.Size {
			return cp, nil, fmt.Errorf("checkpoint blob size mismatch at %q", entry.Path)
		}
		sum := sha256.Sum256(data)
		if !bytes.Equal(sum[:], wantHash) {
			return cp, nil, fmt.Errorf("checkpoint blob hash mismatch at %q", entry.Path)
		}
		total += len(data)
		files[entry.Path] = rewindFileState{path: entry.Path, mode: os.FileMode(entry.Mode), data: data}
	}
	return cp, files, nil
}

func rewindBuildPlan(home, sessionID string, anchorSeq int64, cwd string) (*rewindPlan, error) {
	cp, target, err := rewindLoadCheckpoint(home, sessionID, anchorSeq)
	if err != nil {
		return nil, err
	}
	root, err := rewindValidateRoot(home, cwd)
	if err != nil {
		return nil, err
	}
	if !rewindSamePath(root, cp.Root) {
		return nil, fmt.Errorf("rewind checkpoint belongs to cwd %q, not %q", cp.Root, root)
	}
	currentFiles, err := rewindScanFiles(home, root)
	if err != nil {
		return nil, fmt.Errorf("scan current rewind files: %w", err)
	}
	current := make(map[string]rewindFileState, len(currentFiles))
	for _, file := range currentFiles {
		current[file.path] = file
	}
	plan := &rewindPlan{
		root:    root,
		target:  target,
		current: current,
		changed: make([]string, 0),
		preview: RewindFilesPreview{FilesChanged: make([]string, 0)},
	}
	for path, before := range target {
		after, exists := current[path]
		if exists && bytes.Equal(before.data, after.data) && before.mode.Perm() == after.mode.Perm() {
			continue
		}
		plan.changed = append(plan.changed, path)
		var currentData []byte
		if exists {
			currentData = after.data
		}
		insertions, deletions := rewindLineDiff(currentData, before.data)
		plan.preview.Insertions += insertions
		plan.preview.Deletions += deletions
	}
	for path, after := range current {
		if _, exists := target[path]; exists {
			continue
		}
		plan.changed = append(plan.changed, path)
		insertions, deletions := rewindLineDiff(after.data, nil)
		plan.preview.Insertions += insertions
		plan.preview.Deletions += deletions
	}
	sort.Strings(plan.changed)
	plan.preview.FilesChanged = append(plan.preview.FilesChanged, plan.changed...)
	return plan, nil
}

func rewindSplitLines(data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	lines := bytes.SplitAfter(data, []byte{'\n'})
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func rewindLineDiff(current, target []byte) (insertions, deletions int) {
	a := rewindSplitLines(current)
	b := rewindSplitLines(target)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && bytes.Equal(a[prefix], b[prefix]) {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && bytes.Equal(a[len(a)-1-suffix], b[len(b)-1-suffix]) {
		suffix++
	}
	a = a[prefix : len(a)-suffix]
	b = b[prefix : len(b)-suffix]
	if len(a) == 0 || len(b) == 0 {
		return len(b), len(a)
	}
	// 大文件避免二次方内存/CPU：去掉公共前后缀后按替换块计数。
	if len(a) > rewindMaxDiffCells/len(b) {
		return len(b), len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		clear(curr)
		for j := 1; j <= len(b); j++ {
			if bytes.Equal(a[i-1], b[j-1]) {
				curr[j] = prev[j-1] + 1
			} else if prev[j] >= curr[j-1] {
				curr[j] = prev[j]
			} else {
				curr[j] = curr[j-1]
			}
		}
		prev, curr = curr, prev
	}
	lcs := prev[len(b)]
	return len(b) - lcs, len(a) - lcs
}

type rewindJournalKind uint8

const (
	rewindJournalBackup rewindJournalKind = iota + 1
	rewindJournalInstalled
	rewindJournalMadeDir
	rewindJournalRemovedDir
)

type rewindJournalEntry struct {
	kind   rewindJournalKind
	path   string
	backup string
	mode   os.FileMode
}

func rewindDepth(path string) int { return strings.Count(path, "/") }

func rewindCheckParents(root, rel string) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parentRel := strings.Join(parts[:i], "/")
		parent, err := rewindSecureJoin(root, parentRel)
		if err != nil {
			return err
		}
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe non-directory parent %q", parentRel)
		}
	}
	return nil
}

func rewindMoveToBackup(plan *rewindPlan, txn string, rel string, requireRegular bool, journal *[]rewindJournalEntry, counter *int) error {
	if err := rewindCheckParents(plan.root, rel); err != nil {
		return err
	}
	source, err := rewindSecureJoin(plan.root, rel)
	if err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.IsDir() || (requireRegular && !info.Mode().IsRegular()) {
		return fmt.Errorf("unexpected file type at %q", rel)
	}
	backup := filepath.Join(txn, "backups", fmt.Sprintf("%08d", *counter))
	*counter++
	if err := os.Rename(source, backup); err != nil {
		return fmt.Errorf("back up %q: %w", rel, err)
	}
	*journal = append(*journal, rewindJournalEntry{kind: rewindJournalBackup, path: rel, backup: backup})
	return nil
}

func rewindPrepareParents(plan *rewindPlan, txn, rel string, journal *[]rewindJournalEntry, counter *int) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parentRel := strings.Join(parts[:i], "/")
		parent, err := rewindSecureJoin(plan.root, parentRel)
		if err != nil {
			return err
		}
		info, err := os.Lstat(parent)
		if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if err := rewindMoveToBackup(plan, txn, parentRel, false, journal, counter); err != nil {
				return err
			}
		}
		if err := os.Mkdir(parent, 0o755); err != nil {
			return fmt.Errorf("create parent %q: %w", parentRel, err)
		}
		*journal = append(*journal, rewindJournalEntry{kind: rewindJournalMadeDir, path: parentRel})
	}
	return nil
}

func rewindRollback(plan *rewindPlan, journal []rewindJournalEntry) error {
	var errs []error
	for i := len(journal) - 1; i >= 0; i-- {
		entry := journal[i]
		path, err := rewindSecureJoin(plan.root, entry.path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch entry.kind {
		case rewindJournalInstalled:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove installed %s: %w", entry.path, err))
			}
		case rewindJournalMadeDir:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove created directory %s: %w", entry.path, err))
			}
		case rewindJournalRemovedDir:
			if err := os.Mkdir(path, entry.mode.Perm()); err != nil && !errors.Is(err, os.ErrExist) {
				errs = append(errs, fmt.Errorf("recreate directory %s: %w", entry.path, err))
			}
		case rewindJournalBackup:
			if err := os.Rename(entry.backup, path); err != nil {
				errs = append(errs, fmt.Errorf("restore backup %s: %w", entry.path, err))
			}
		}
	}
	return errors.Join(errs...)
}

func rewindAbort(plan *rewindPlan, txn string, journal []rewindJournalEntry, cause error) (RewindFilesPreview, error) {
	if rollbackErr := rewindRollback(plan, journal); rollbackErr != nil {
		return RewindFilesPreview{}, fmt.Errorf("rewind restore failed: %v; rollback incomplete: %v; backups retained in %s", cause, rollbackErr, txn)
	}
	if err := os.RemoveAll(txn); err != nil {
		return RewindFilesPreview{}, errors.Join(fmt.Errorf("rewind restore failed and rolled back: %w", cause), fmt.Errorf("clean transaction: %w", err))
	}
	return RewindFilesPreview{}, fmt.Errorf("rewind restore failed; original files rolled back: %w", cause)
}

func rewindApplyPlan(plan *rewindPlan, beforeInstall func(string) error) (RewindFilesPreview, error) {
	txn, err := os.MkdirTemp(plan.root, ".dsh-rewind-")
	if err != nil {
		return RewindFilesPreview{}, fmt.Errorf("create rewind transaction: %w", err)
	}
	journal := make([]rewindJournalEntry, 0)
	for _, dir := range []string{filepath.Join(txn, "stage"), filepath.Join(txn, "backups")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return rewindAbort(plan, txn, journal, err)
		}
	}
	stage := make(map[string]string)
	for i, rel := range plan.changed {
		before, exists := plan.target[rel]
		if !exists {
			continue
		}
		path := filepath.Join(txn, "stage", fmt.Sprintf("%08d", i))
		if err := rewindWriteNewFile(path, before.data, 0o600); err != nil {
			return rewindAbort(plan, txn, journal, fmt.Errorf("stage %s: %w", rel, err))
		}
		stage[rel] = path
	}

	currentChanged := make([]string, 0, len(plan.changed))
	for _, rel := range plan.changed {
		if _, exists := plan.current[rel]; exists {
			currentChanged = append(currentChanged, rel)
		}
	}
	sort.Slice(currentChanged, func(i, j int) bool {
		if rewindDepth(currentChanged[i]) != rewindDepth(currentChanged[j]) {
			return rewindDepth(currentChanged[i]) > rewindDepth(currentChanged[j])
		}
		return currentChanged[i] < currentChanged[j]
	})
	backupCounter := 0
	for _, rel := range currentChanged {
		if err := rewindMoveToBackup(plan, txn, rel, true, &journal, &backupCounter); err != nil {
			return rewindAbort(plan, txn, journal, err)
		}
	}

	targetChanged := make([]string, 0, len(stage))
	for rel := range stage {
		targetChanged = append(targetChanged, rel)
	}
	sort.Slice(targetChanged, func(i, j int) bool {
		if rewindDepth(targetChanged[i]) != rewindDepth(targetChanged[j]) {
			return rewindDepth(targetChanged[i]) < rewindDepth(targetChanged[j])
		}
		return targetChanged[i] < targetChanged[j]
	})
	for _, rel := range targetChanged {
		if err := rewindPrepareParents(plan, txn, rel, &journal, &backupCounter); err != nil {
			return rewindAbort(plan, txn, journal, err)
		}
		target, err := rewindSecureJoin(plan.root, rel)
		if err != nil {
			return rewindAbort(plan, txn, journal, err)
		}
		info, err := os.Lstat(target)
		if err == nil {
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				if err := os.Remove(target); err != nil {
					return rewindAbort(plan, txn, journal, fmt.Errorf("remove empty directory blocking %s: %w", rel, err))
				}
				journal = append(journal, rewindJournalEntry{kind: rewindJournalRemovedDir, path: rel, mode: info.Mode().Perm()})
			} else {
				if err := rewindMoveToBackup(plan, txn, rel, false, &journal, &backupCounter); err != nil {
					return rewindAbort(plan, txn, journal, err)
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return rewindAbort(plan, txn, journal, err)
		}
		if beforeInstall != nil {
			if err := beforeInstall(rel); err != nil {
				return rewindAbort(plan, txn, journal, err)
			}
		}
		if err := os.Rename(stage[rel], target); err != nil {
			return rewindAbort(plan, txn, journal, fmt.Errorf("install %s: %w", rel, err))
		}
		journal = append(journal, rewindJournalEntry{kind: rewindJournalInstalled, path: rel})
		if err := os.Chmod(target, plan.target[rel].mode.Perm()); err != nil {
			return rewindAbort(plan, txn, journal, fmt.Errorf("restore mode for %s: %w", rel, err))
		}
	}
	if err := os.RemoveAll(txn); err != nil {
		return plan.preview, fmt.Errorf("files restored but rewind backup cleanup failed: %w", err)
	}
	return plan.preview, nil
}
