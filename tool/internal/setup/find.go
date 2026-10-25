// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 本文件从 Go 自己输出的 dry-run 计划恢复“究竟会编译哪些源码”。
// 这比只读 go.mod 更细：go.mod 管模块版本，compile 命令则包含包路径、构建标签
// 筛选后的文件、测试变体及 cgo 生成输入，是后续规则匹配的实际依据。
package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/util"
)

const maxBuildPlanBufferSize = 10 * 1024 * 1024 // 10MB

//nolint:gochecknoglobals // allows us to mock exec.CommandContext in tests
var execCommandContext = exec.CommandContext

// Dependency 表示一次编译输入中的 Go 包，而非整个第三方 module。
// ImportPath 来自编译器 -p，Sources 使用绝对路径；同一个 import path 在普通构建
// 和测试构建中可能有不同源文件集合，因此不能只按 ImportPath 对编译计划去重。
type Dependency struct {
	ImportPath string
	Version    string
	Sources    []string
	CgoFiles   map[string]string
}

func (d *Dependency) String() string {
	if d.Version == "" {
		return fmt.Sprintf("{%s: %v}", d.ImportPath, d.Sources)
	}
	return fmt.Sprintf("{%s@%s: %v}", d.ImportPath, d.Version, d.Sources)
}

// parseCdDir extracts the directory path from a "cd" command line.
func parseCdDir(line string) (string, bool) {
	const prefix = "cd "
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		return "", false
	}
	dir := strings.TrimSpace(line[len(prefix):])
	if dir == "" {
		return "", false
	}
	return dir, true
}

// findCommands scans the build plan log and returns relevant commands
// (cd, cgo, and compile) for processing by findDeps.
func findCommands(buildPlanLog *os.File) ([]string, error) {
	scanner, err := util.NewFileScanner(buildPlanLog, maxBuildPlanBufferSize)
	if err != nil {
		return nil, err
	}

	var commands []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if util.IsWindows() {
			line = strings.ReplaceAll(line, `\\`, `\`)
		}
		line = filepath.ToSlash(line)

		if _, ok := parseCdDir(line); ok || util.IsCgoCommand(line) ||
			util.IsCompileCommandWithArgs(util.SplitCompileCmds(line)) {
			commands = append(commands, line)
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, ex.Wrapf(err, "failed to parse build plan log")
	}
	return commands, nil
}

// disableJSONOutput keeps the build plan on stderr as shell commands.
// Retain the flag position: removing a flag after go test's package list
// could turn a test-binary argument into another package target.
func disableJSONOutput(subcommand string, args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for _, arg := range classifyArgs(subcommand, args) {
		if arg.Kind == ArgBuildFlag && arg.FlagName == flagJSON {
			out[arg.Index] = "-json=false"
		}
	}
	return out
}

// listBuildPlan lists the build plan by running `go build -a -x -n`
// and then filtering the commands (cd, cgo, compile) from the build plan log.
func listBuildPlan(ctx context.Context, subcommand string, cmdArgs []string) ([]string, error) {
	// -n 只打印不执行，-x 输出工具命令，-a 让已缓存的包也出现在计划中。
	// 因此这里不是先完整编译一次；真正编译发生在 setup 完成后的 toolexec 阶段。
	const buildPlanLogName = "build-plan.log"

	logger := util.LoggerFromContext(ctx)

	// Create a build plan log file in the temporary directory
	buildPlanLog, err := os.Create(util.GetBuildTemp(buildPlanLogName))
	if err != nil {
		return nil, ex.Wrapf(err, "failed to create build plan log file")
	}
	defer buildPlanLog.Close()
	// The dry run lists the compile commands the toolchain would issue. `go test`
	// needs its own plan because only it surfaces the test-augmented, external
	// test, and test-main compiles that is_test gates on. `go install` shares
	// `go build`'s compile plan, so it stays on the build verb.
	planVerb := subcmdBuild
	if subcommand == subcmdTest {
		planVerb = subcmdTest
	}
	// The full command is: "go build/test [-C dir] -a -x -n {...}"
	planArgs := disableJSONOutput(subcommand, cmdArgs)
	planFlags := []string{"-a", "-x", "-n"}
	args := make([]string, 0, len(planArgs)+len(planFlags)+1)
	args = append(args, planVerb)
	args = append(args, addBuildFlags(planArgs, planFlags...)...)
	logger.InfoContext(ctx, "go build command", "args", args)

	cmd := execCommandContext(ctx, "go", args...)
	// This is a little anti-intuitive as the error message is not printed to
	// the stderr, instead it is printed to the stdout, only the build tool
	// knows the reason why.
	cmd.Stdout = os.Stdout
	cmd.Stderr = buildPlanLog
	// @@Note that dir should not be set, as the dry build should be run in the
	// same directory as the original build command
	cmd.Dir = ""
	err = cmd.Run()
	if err != nil {
		// Read the build plan log to see what went wrong
		logContent, _ := os.ReadFile(util.GetBuildTemp(buildPlanLogName))
		return nil, ex.Wrapf(err, "failed to run build plan: \n%s", string(logContent))
	}

	// Find compile commands from build plan log
	compileCmds, err := findCommands(buildPlanLog)
	if err != nil {
		return nil, err
	}
	logger.DebugContext(ctx, "Found compile commands", "compileCmds", compileCmds)
	return compileCmds, nil
}

const (
	cgoSuffix = ".cgo1.go"
	goSuffix  = ".go"
)

// resolveCgoFile maps a CGO-generated file back to its original source.
func resolveCgoFile(cgoFile, sourceDir string) (string, error) {
	if cgoFile == "" || sourceDir == "" {
		return "", ex.Newf("cgoFile and sourceDir cannot be empty, cgoFile: %q, sourceDir: %q", cgoFile, sourceDir)
	}
	baseName := filepath.Base(cgoFile)
	if !strings.HasSuffix(baseName, cgoSuffix) {
		return "", ex.Newf("file %s is not a CGO (%s) generated file", cgoFile, cgoSuffix)
	}
	originalBase := strings.TrimSuffix(baseName, cgoSuffix) + goSuffix
	abs := filepath.Join(sourceDir, originalBase)
	if !util.PathExists(abs) {
		return "", ex.Newf("file %s does not exist", abs)
	}
	return abs, nil
}

var versionRegexp = regexp.MustCompile(`@v\d+\.\d+\.\d+([-+][^/]*)?/`)

func findModVersion(path string) string {
	// 版本由模块缓存路径中的 @vX.Y.Z 提取，而非根据包名猜测。
	// 标准库、主模块、本地 replace 或 vendor 源码通常没有这一段，返回空版本；
	// 有版本下限的规则不会把“无法确认版本”当作符合条件，排查未命中时要检查来源。
	version := versionRegexp.FindString(filepath.ToSlash(path))
	if version == "" {
		return ""
	}
	return version[1 : len(version)-1]
}

// findGoSources extracts Go source files from compile command arguments,
// resolving CGO files using the provided objDir->sourceDir mapping.
func findGoSources(ctx context.Context, args []string, cgoObjDirs map[string]string) (*Dependency, error) {
	// dry-run 尚未生成 cgo 的 .cgo1.go 文件，因此不能仅凭文件不存在就丢弃该包。
	// 先把生成文件映回原始 .go，再保留原文件 → cgo 文件名的映射供编译阶段使用。
	logger := util.LoggerFromContext(ctx)

	dep := &Dependency{
		ImportPath: util.FindFlagValue(args, "-p"),
		Sources:    make([]string, 0),
		CgoFiles:   make(map[string]string),
	}
	util.Assert(dep.ImportPath != "", "import path is empty")

	// Find the go files belong to the package as dependency sources
	for _, arg := range args {
		if !util.IsGoFile(arg) {
			continue
		}
		if !util.PathExists(arg) {
			// Try to resolve as CGO generated file
			objDir := util.NormalizePath(filepath.Dir(arg))
			sourceDir, ok := cgoObjDirs[objDir]
			if !ok {
				logger.DebugContext(ctx, "Skip generated file - unknown objdir", "file", arg, "objDir", objDir)
				continue
			}
			originalAbsFile, err := resolveCgoFile(arg, sourceDir)
			if err != nil {
				logger.DebugContext(ctx, "Skip generated file", "file", arg, "error", err)
				continue
			}
			dep.CgoFiles[originalAbsFile] = filepath.Base(arg)
			dep.Sources = append(dep.Sources, originalAbsFile)
			logger.DebugContext(ctx, "Resolved CGO source", "cgo", arg, "original", originalAbsFile)
			continue
		}

		// This is a Go source file, add it to the dependency sources
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, ex.Wrapf(err, "failed to get absolute path of source file %s", arg)
		}
		dep.Sources = append(dep.Sources, abs)
	}
	// We need to skip it as it is not part of the instrumentation target
	if len(dep.Sources) > 0 {
		dep.Version = findModVersion(dep.Sources[0])
	}
	return dep, nil
}

// findDeps finds dependencies by listing the build plan.
func findDeps(ctx context.Context, subcommand string, cmdArgs []string) ([]*Dependency, error) {
	// 按计划顺序处理 cd/cgo/compile：cd 提供 cgo 原目录，cgo 提供 -objdir，
	// 随后的 compile 才能把尚不存在的生成文件追溯到可读取的原始源码。
	logger := util.LoggerFromContext(ctx)

	buildPlan, err := listBuildPlan(ctx, subcommand, cmdArgs)
	if err != nil {
		return nil, err
	}

	var (
		deps       []*Dependency
		cgoObjDirs = make(map[string]string)
		currentDir string
		// A package can be compiled more than once with the same sources,
		// e.g. once with and once without a PGO profile when only some of the
		// main packages have a default.pgo. Keep one of them.
		seen = make(map[string]bool)
	)

	for _, cmd := range buildPlan {
		if dir, ok := parseCdDir(cmd); ok {
			currentDir = dir
			continue
		}

		args := util.SplitCompileCmds(cmd)
		if util.IsCompileCommandWithArgs(args) {
			dep, err1 := findGoSources(ctx, args, cgoObjDirs)
			if err1 != nil {
				return nil, err1
			}
			key := dep.ImportPath + "\x00" + strings.Join(dep.Sources, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			deps = append(deps, dep)
			logger.InfoContext(ctx, "Found dependency", "dep", dep)
		} else if util.IsCgoCommand(cmd) && currentDir != "" {
			objDir := util.FindFlagValue(args, "-objdir")
			util.Assert(objDir != "", "sanity check")
			cgoObjDirs[util.NormalizePath(objDir)] = currentDir
			logger.DebugContext(ctx, "Found CGO objdir mapping", "objDir", objDir, "sourceDir", currentDir)
		}
	}
	return deps, nil
}
