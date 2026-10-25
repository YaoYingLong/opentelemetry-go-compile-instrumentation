// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 状态管理使构建前的临时修改可以回滚：文件内容快照 + 原始存在性清单。
// 这不是 git 工作区回退，不会依据版本库 HEAD 覆盖用户未提交内容；
// 恢复基准是每个文件第一次被 Track 时的真实状态。
package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/util"
)

const (
	stateDir      = "state"
	stateFileName = "state.json"
	goWorkOff     = "off"
)

// stateManager tracks the original state of files so they can later be restored.
//
// Files that exist when tracked are snapshotted into the build state directory.
// Files that do not exist when tracked are recorded and removed during Revert if
// they are later created.
//
// stateManager is not safe for concurrent use.
// 中文导读：map 的 bool 同时区分“原来存在”和“原来不存在”；
// 因此查询时要用 value, ok := map[key]，不能把 false 误认为“尚未登记”。
// 跨进程的构建锁负责保护整个构建生命周期，本结构自身不提供 goroutine 并发安全。
type stateManager struct {
	files map[string]bool // true = existed when tracked
}

// newStateManager returns an empty stateManager.
func newStateManager() *stateManager {
	return &stateManager{
		files: make(map[string]bool),
	}
}

// loadStateManager loads a previously committed stateManager from disk.
//
// If no state has been committed, it returns (nil, nil).
//
//nolint:nilnil // nil is returned when the state file does not exist
func loadStateManager() (*stateManager, error) {
	f := util.GetBuildTemp(stateFileName)
	if !util.PathExists(f) {
		return nil, nil
	}

	file, err := os.Open(f)
	if err != nil {
		return nil, ex.Wrapf(err, "failed to open state file %s", f)
	}
	defer file.Close()

	var entries []string
	if err = json.NewDecoder(file).Decode(&entries); err != nil {
		return nil, ex.Wrapf(err, "failed to decode state JSON from file %s", f)
	}

	s := newStateManager()
	for _, entry := range entries {
		if e, ok := strings.CutPrefix(entry, "-"); ok {
			s.files[e] = false
		} else {
			s.files[entry] = true
		}
	}

	return s, nil
}

// 使用包内私有空结构体作为 context key，避免别的包用同名字符串覆盖这个值。
// struct{} 不携带业务字段；context 在这里只传播当前构建的状态管理器指针。
type stateManagerKey struct{}

// contextWithStateManager returns a copy of ctx containing s.
func contextWithStateManager(ctx context.Context, s *stateManager) context.Context {
	return context.WithValue(ctx, stateManagerKey{}, s)
}

// stateManagerFromContext returns the stateManager stored in ctx.
//
// If ctx does not contain a stateManager, a new empty stateManager is returned
// along with false.
func stateManagerFromContext(ctx context.Context) (*stateManager, bool) {
	s, ok := ctx.Value(stateManagerKey{}).(*stateManager)
	if !ok {
		return newStateManager(), false
	}
	return s, true
}

func getBackupFiles(ctx context.Context, moduleDirs []string) ([]string, error) {
	// 自动 pin 可能修改 go.mod/go.sum 和组件清单，工作区模式还可能更新 go.work.sum。
	// go.sum 不存在也要登记，这样本次新生成的文件会在清理时删除。
	var files []string

	dirs := normalizeModuleDirs(moduleDirs)
	// Find all go.mod, go.sum, and tool files
	for _, moduleDir := range dirs {
		goModFile := filepath.Join(moduleDir, goModFileName)
		goSumFile := filepath.Join(moduleDir, "go.sum")
		canonical := filepath.Join(moduleDir, toolFileCanonical)
		alias := filepath.Join(moduleDir, toolFileAlias)

		if util.PathExists(goModFile) {
			files = append(files, goModFile)
			files = append(files, goSumFile)

			// If otelc.tool.go exists, use it (it may get modified)
			// Otherwise, use the canonical path (it may get generated or modified)
			toolFile := canonical
			if !util.PathExists(canonical) && util.PathExists(alias) {
				toolFile = alias
			}

			files = append(files, toolFile)
		}
	}

	// Find go.work.sum if go.work exists
	goWorkCmd := exec.CommandContext(ctx, "go", "env", "GOWORK")
	goWorkOutput, err := goWorkCmd.Output()
	if err != nil {
		return nil, ex.Wrapf(err, "failed to get GOWORK environment variable")
	}
	goWorkPath := strings.TrimSpace(string(goWorkOutput))
	if goWorkPath != "" && goWorkPath != goWorkOff {
		goWorkSumPath := filepath.Join(filepath.Dir(goWorkPath), "go.work.sum")
		files = append(files, goWorkSumPath)
	}

	return files, nil
}

func stateSnapshotPath(path string) string {
	// 多模块工程可能同时有多个 go.mod；只按 basename 备份会覆盖彼此。
	// 文件名保留可读部分，再加完整路径的 SHA-256 摘要，区分不同目录的同名文件。
	p := filepath.Clean(path)
	sum := sha256.Sum256([]byte(p))
	return filepath.Base(p) + "." + hex.EncodeToString(sum[:])
}

// TrackAll calls Track for each path.
func (s *stateManager) TrackAll(paths ...string) error {
	var err error
	for _, path := range paths {
		err = ex.Join(err, s.Track(path))
	}
	return err
}

// Track records the current state of path.
//
// If path exists, it is snapshotted and will be restored by Revert.
// If path does not exist, it is recorded so Revert will remove it if it is
// later created.
//
// The updated state is persisted to disk before Track returns, so a build
// killed at any later point (SIGKILL, OOM, CI cancellation) leaves behind a
// manifest from which `otelc cleanup` or the next build can restore the tree.
//
// Duplicate calls for the same path are ignored.
func (s *stateManager) Track(path string) error {
	// 每次成功登记都立即持久化，不等整个 setup 完成才保存。
	// 返回后调用方才能安全修改文件；若后来进程异常退出，独立 cleanup 仍能找到清单。
	// 重复 Track 保留最初状态，避免把已经修改后的文件再次当作原始快照。
	abs, err := filepath.Abs(path)
	if err != nil {
		return ex.Wrapf(err, "failed to get absolute path for %s", path)
	}

	abs = filepath.Clean(abs)
	if _, ok := s.files[abs]; ok {
		return nil
	}

	// If the file doesn't exist, mark it for removal
	if !util.PathExists(abs) {
		s.files[abs] = false
		return s.Commit()
	}

	// If the file exists, snapshot it
	dst := filepath.Join(util.GetBuildTemp(stateDir), stateSnapshotPath(abs))
	if err = util.CopyFile(abs, dst); err != nil {
		return err
	}

	s.files[abs] = true
	return s.Commit()
}

// Commit persists the tracked state to disk so it can be restored by a future
// process. The manifest is written atomically (temp file + rename) so a crash
// mid-write never leaves a truncated manifest behind.
//
// Track calls Commit automatically; calling it directly is only needed to
// re-persist after external changes.
func (s *stateManager) Commit() error {
	// 清单中普通路径表示“恢复快照”，前缀 '-' 表示“原来不存在，应删除”。
	// map 遍历顺序不稳定，排序后再输出，使磁盘结果稳定且便于比较。
	if len(s.files) == 0 {
		return nil
	}

	entries := make([]string, 0, len(s.files))

	for path, exists := range s.files {
		if exists {
			entries = append(entries, path)
		} else {
			entries = append(entries, "-"+path)
		}
	}

	// Sort the entries for deterministic behavior
	sort.Strings(entries)

	bs, err := json.Marshal(entries)
	if err != nil {
		return ex.Wrapf(err, "failed to marshal state to JSON")
	}

	f := util.GetBuildTemp(stateFileName)
	if err = os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return ex.Wrapf(err, "failed to create build temp directory")
	}

	return util.WriteFileAtomic(f, bs)
}

// Discard removes the persisted manifest and snapshots. Call it only after a
// successful Revert: the state is consumed, and leaving it behind would let a
// later `otelc cleanup` re-apply snapshots from a finished build.
func (s *stateManager) Discard() error {
	// 先成功 Revert，再删除恢复依据；恢复失败时必须保留快照，不能为了清理干净而丢失数据。
	if len(s.files) == 0 {
		return nil
	}

	var err error
	if rmErr := os.Remove(util.GetBuildTemp(stateFileName)); rmErr != nil && !os.IsNotExist(rmErr) {
		err = ex.Join(err, rmErr)
	}
	if rmErr := os.RemoveAll(util.GetBuildTemp(stateDir)); rmErr != nil {
		err = ex.Join(err, rmErr)
	}
	return err
}

// Revert restores all tracked files to the state they were in when tracked.
//
// Files that originally existed are restored from their snapshots. Files that
// did not exist when tracked are removed if they exist.
func (s *stateManager) Revert() error {
	// ex.Join 累积错误而不是首错即停：一个文件恢复失败，仍尝试恢复其他文件。
	// 调用方据最终错误决定是否能 Discard；这个函数自己不删除状态清单。
	if len(s.files) == 0 {
		return nil
	}

	var err error

	stateDir := util.GetBuildTemp(stateDir)

	for path, existed := range s.files {
		if !existed {
			if util.PathExists(path) {
				err = ex.Join(err, os.Remove(path))
			}
			continue
		}

		src := filepath.Join(stateDir, stateSnapshotPath(path))
		err = ex.Join(err, util.CopyFile(src, path))
	}

	return err
}
