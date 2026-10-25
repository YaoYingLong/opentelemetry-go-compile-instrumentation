// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package setup 实现编译期插桩的第一阶段：分析用户构建、选择组件、匹配规则，
// 并生成第二阶段需要的运行时导入文件和 matched.json。这里的“运行时文件”仍是
// 在构建前生成的 Go 源码；真正改写目标函数由 internal/instrument 在编译回调中完成。
// 建议阅读顺序：GoBuild → setupLocked → findDeps → matchDeps → addDeps → store。
package setup

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/internal/instrument"
	"go.opentelemetry.io/otelc/tool/internal/pkgload"
	"go.opentelemetry.io/otelc/tool/internal/rule"
	"go.opentelemetry.io/otelc/tool/util"
	"golang.org/x/tools/go/packages"
)

// setupPhase 保存一次准备过程的公共信息，不是最终应用中的运行时状态。
// *packages.Package 是 go/packages 的包描述：import path、模块、源码和依赖图等；
// 它不同于一个 Go module，也不同于某次 compile 命令对应的 Dependency。
type setupPhase struct {
	logger          *slog.Logger
	ruleConfig      string
	buildPackages   []*packages.Package
	buildFlags      []string
	rootModulePaths []string
	// resolvedNames maps an import path to a package name, built from
	// buildPackages' dependency graph.
	// import path 的末段不一定等于 package 声明，故后续签名匹配需要真实包名表。
	resolvedNames map[string]string
}

func (sp *setupPhase) Info(msg string, args ...any)  { sp.logger.Info(msg, args...) }
func (sp *setupPhase) Error(msg string, args ...any) { sp.logger.Error(msg, args...) }
func (sp *setupPhase) Warn(msg string, args ...any)  { sp.logger.Warn(msg, args...) }
func (sp *setupPhase) Debug(msg string, args ...any) { sp.logger.Debug(msg, args...) }

// keepForDebug copies the file to the build temp directory for debugging.
// Error is tolerated as it's not critical.
func keepForDebug(ctx context.Context, srcPath string) {
	logger := util.LoggerFromContext(ctx)

	escape := func(s string) string {
		s = strings.ReplaceAll(s, "/", "_")
		s = strings.ReplaceAll(s, ".", "_")
		return s
	}

	var name string
	if filepath.Clean(filepath.Dir(srcPath)) == filepath.Clean(util.GetOtelcWorkDir()) {
		name = "main"
	} else {
		name = escape(filepath.Base(filepath.Dir(srcPath)))
	}

	dstPath := filepath.Join(util.GetBuildTemp("debug"), name, filepath.Base(srcPath))
	if err := util.CopyFile(srcPath, dstPath); err != nil {
		logger.WarnContext(ctx, "failed to record added file", "path", srcPath, "error", err)
	}
}

// This function can be used to check if the setup has been completed.
func isSetup() bool {
	// 当前始终返回 false：不要把下面的短路分支理解成已经实现了 setup 缓存。
	// TODO: Implement Task
	return false
}

// Go subcommands that otelc wraps with toolexec instrumentation.
const (
	subcmdBuild   = "build"
	subcmdInstall = "install"
	subcmdTest    = "test"

	goModFileName = "go.mod"
	vendorDirName = "vendor"
	flagMod       = "-mod"
)

// Go command flags that the argument scanners treat specially.
const (
	// flagArgs separates the go command's own arguments from the ones it
	// passes through to the test binary.
	flagArgs          = "-args"
	flagArgsDashDash  = "--args"
	delimiterDashDash = "--"
	// flagJSON makes the go command report its output as JSON events.
	flagJSON = "-json"
)

func lastFileTargetIndex(subcommand string, args []string) int {
	last := -1
	for _, arg := range classifyArgs(subcommand, args) {
		if arg.Kind == ArgTarget && filepath.Ext(arg.Raw) == ".go" {
			last = arg.Index
		}
	}
	return last
}

// getBuildPackages loads all packages from the otelc go build/install/test or otelc setup command arguments.
// If no package patterns are found in args, defaults to loading the current directory package.
// subcommand is the go subcommand args from "build", "install", or "test".
// The args parameter should be the go build/install/test command arguments (e.g., ["-a", "./cmd"]).
// It returns two lists:
//   - the build packages: the real packages the go command builds. A test load
//     also returns test packages ("p [p.test]" variants, external "p_test"
//     packages, and the synthesized "p.test" binaries). They are excluded,
//     because otelc.runtime.go for one of them would declare "package p_test"
//     next to "p.go" and fail the build.
//   - all packages: the build packages plus those test packages. Only this list
//     sees them, because they carry imports that only _test.go files use.
//
// Returns an error if package loading fails or if invalid patterns are provided.
// For example:
//   - args ["-a", "./cmd"] returns packages for "./cmd"
//   - args ["-a", "cmd"] returns packages for the "cmd" package in the module
//   - args ["-a", ".", "./cmd"] returns packages for both "." and "./cmd"
//   - args [] returns packages for "."
//
// 中文导读：这里加载“构建入口及其依赖图”，供模块定位和包名解析使用。
// 后面的 findDeps 则从 dry-run 编译计划获取每次编译实际输入的源文件；
// 两者互补，尤其 go test 的包变体不能直接当作普通包写入生成文件。
// packages.Need* 是位标志，用按位或组合要加载的信息，避免无谓加载完整类型信息。
//nolint:revive // needed to balance confusing-results and nonamedreturns linters
func getBuildPackages(
	ctx context.Context,
	subcommand string,
	args []string,
) ([]*packages.Package, []*packages.Package, error) {
	logger := util.LoggerFromContext(ctx)
	// NeedForTest marks the test packages, so the loop below can split them
	// from the real build packages.
	mode := packages.NeedName | packages.NeedFiles | packages.NeedModule |
		packages.NeedImports | packages.NeedDeps | packages.NeedForTest
	tests := subcommand == subcmdTest

	pkgTargets, fileTargets, err := splitBuildTargets(subcommand, args)
	if err != nil {
		return nil, nil, err
	}
	buildFlags := extractBuildFlags(subcommand, args)

	var (
		pkgs    []*packages.Package
		loadErr error
	)
	switch {
	case len(fileTargets) > 0:
		pkgs, loadErr = pkgload.LoadPackages(ctx, mode, buildFlags, tests, fileTargets...)
		if loadErr != nil {
			return nil, nil, loadErr
		}

		if len(pkgs) > 1 {
			return nil, nil, ex.New("multiple packages found for file targets")
		}
	case len(pkgTargets) > 0:
		pkgs, loadErr = pkgload.LoadPackages(ctx, mode, buildFlags, tests, pkgTargets...)
		if loadErr != nil {
			return nil, nil, loadErr
		}
	default:
		pkgs, loadErr = pkgload.LoadPackages(ctx, mode, buildFlags, tests, ".")
		if loadErr != nil {
			return nil, nil, loadErr
		}
	}

	// A test load sets ForTest on every test package it adds. The synthesized
	// test binaries ("p.test") have no marker of their own, but each one is the
	// ForTest value of a test package plus a ".test" suffix.
	testBinaries := make(map[string]bool, len(pkgs))
	for _, pkg := range pkgs {
		if pkg.ForTest != "" {
			testBinaries[pkg.ForTest+".test"] = true
		}
	}

	buildPkgs := make([]*packages.Package, 0, len(pkgs))
	allPkgs := make([]*packages.Package, 0, len(pkgs))
	for _, pkg := range pkgs {
		// file-based builds use synthetic "command-line-arguments" packages
		if len(pkg.Errors) > 0 || (pkg.Module == nil && pkg.PkgPath != pkgload.CommandLineArgumentsPackage) {
			logger.DebugContext(ctx, "skipping package", "name", pkg.Name, "errors", pkg.Errors, "args", args)
			continue
		}

		allPkgs = append(allPkgs, pkg)
		if pkg.ForTest == "" && !testBinaries[pkg.ID] {
			buildPkgs = append(buildPkgs, pkg)
		}
	}

	if len(buildPkgs) == 0 {
		return nil, nil, ex.New("no valid packages found in build targets")
	}

	return buildPkgs, allPkgs, nil
}

//nolint:revive // if we add named returns then nonamedreturns will complain
func splitBuildTargets(subcommand string, args []string) ([]string, []string, error) {
	classified := classifyArgs(subcommand, args)
	var pkgs, files []string

	for i, a := range classified {
		if (a.Kind == ArgBuildFlag || a.Kind == ArgTestFlag) && !a.HasValue && takesValue(a.FlagName) {
			if i == len(classified)-1 ||
				(i+1 < len(classified) && classified[i+1].Kind != ArgBuildFlagValue && classified[i+1].Kind != ArgTestFlagValue) {
				return nil, nil, ex.Newf("flag %q requires a value", a.Raw)
			}
		}

		if a.Kind == ArgTarget {
			if filepath.Ext(a.Raw) == ".go" {
				files = append(files, a.Raw)
			} else {
				pkgs = append(pkgs, a.Raw)
			}
		}
	}

	if len(files) > 0 && len(pkgs) > 0 {
		return nil, nil, ex.New("cannot mix .go files and packages")
	}

	if len(files) > 0 {
		// All named .go files must live in one directory; compare each against
		// the first.
		dir, err := filepath.Abs(filepath.Dir(files[0]))
		if err != nil {
			return nil, nil, ex.Wrapf(err, "failed to get absolute path for directory containing files")
		}

		for _, f := range files[1:] {
			fdir, err2 := filepath.Abs(filepath.Dir(f))
			if err2 != nil {
				return nil, nil, ex.Wrapf(err2, "failed to get absolute path for directory containing file %s", f)
			}

			if fdir != dir {
				return nil, nil, ex.New("named files must all be in one directory")
			}
		}
	}

	return pkgs, files, nil
}

func rootModulePaths(ctx context.Context, pkgs []*packages.Package) ([]string, error) {
	// 一个构建可跨多个模块。$root 应相对这些入口所属模块展开，而非仅相对当前目录。
	roots := make(map[string]bool)
	for _, pkg := range pkgs {
		if pkg.Module != nil && pkg.Module.Path != "" {
			roots[pkg.Module.Path] = true
			continue
		}
		pkgDir := pkgload.PackageDir(pkg)
		if pkgDir == "" {
			continue
		}
		mod, err := pkgload.ResolveModule(ctx, pkgDir)
		if err != nil {
			return nil, err
		}
		if mod.Path != "" {
			roots[mod.Path] = true
		}
	}
	return slices.Sorted(maps.Keys(roots)), nil
}

// generateRuntimePerPackage generates the injected hook code (otelc.runtime.go)
// for every buildable package.
func (sp *setupPhase) generateRuntimePerPackage(
	ctx context.Context,
	pkgs []*packages.Package,
	matched []*rule.InstRuleSet,
) error {
	// 每个真实构建入口包都接收自己的生成文件；_test 包变体已在 getBuildPackages 排除。
	// 文件使用入口包的 package 名，导入已匹配的 hook，使链接器能看到对应运行时代码。
	for _, pkg := range pkgs {
		pkgDir := pkgload.PackageDir(pkg)
		if pkgDir == "" {
			sp.Warn("skipping package without Go files", "package", pkg.PkgPath)
			continue
		}

		// Introduce additional hook code by generating otelc.runtime.go
		if err := sp.addDeps(ctx, matched, runtimePackage{
			dir:        pkgDir,
			importPath: sp.runtimeImportPath(ctx, pkg, pkgDir),
			name:       pkg.Name,
		}); err != nil {
			return ex.Wrapf(err, "adding deps for package at %s", pkgDir)
		}
	}

	return nil
}

// runtimeImportPath returns the import path of the package that receives a
// generated runtime file. A file target loads one synthetic
// "command-line-arguments" package, so the real path comes from the module that
// owns the directory. If the lookup fails, runtimeImportPath returns the
// synthetic path, and a build outside a module continues to work.
func (sp *setupPhase) runtimeImportPath(ctx context.Context, pkg *packages.Package, pkgDir string) string {
	if pkg.PkgPath != pkgload.CommandLineArgumentsPackage {
		return pkg.PkgPath
	}

	importPath, err := resolveImportPath(ctx, sp.buildFlags, pkgDir)
	if err != nil {
		// Expected outside a module, so this is not a warning on its own.
		sp.Debug("cannot derive the import path of a file target", "dir", pkgDir, "error", err)
		return pkg.PkgPath
	}

	return importPath
}

// resolveImportPath returns the canonical import path of the package in pkgDir.
// resolveImportPath loads pkgDir as a package pattern, so the result is the real
// import path and not the synthetic path that a file target carries.
//
// The build flags are the ones of the build, unchanged. Build tags can exclude
// every file in pkgDir, and relative -modfile or -overlay values resolve against
// the -C directory of the build, so pkgDir cannot replace that directory.
func resolveImportPath(ctx context.Context, buildFlags []string, pkgDir string) (string, error) {
	pkgs, err := pkgload.LoadPackages(ctx, packages.NeedName, buildFlags, false, pkgDir)
	if err != nil {
		return "", err
	}
	if len(pkgs) == 0 || len(pkgs[0].Errors) > 0 || pkgs[0].PkgPath == "" {
		return "", ex.Newf("cannot resolve the import path of the package in %s", pkgDir)
	}

	return pkgs[0].PkgPath, nil
}

// Setup prepares the environment for further instrumentation. It runs
// under the build lock; when invoked from GoBuild the surrounding lock is
// reused via the context marker instead of re-acquired.
func Setup(ctx context.Context, cmd *cli.Command) error {
	// 共享同一 otelc 工作目录的构建通过文件锁串行化，保护共用的临时状态。
	// GoBuild 已持锁时，context 中的标记让嵌套 Setup 复用锁，避免自身死锁。
	return withBuildLock(ctx, func(ctx context.Context) error {
		return setupLocked(ctx, cmd)
	})
}

func setupLocked(ctx context.Context, cmd *cli.Command) error {
	// 第一阶段主流程：保留原始构建语义 → 加载包图 → 自动 pin（若未自选规则）→
	// 读取编译计划 → 匹配规则 → 补齐 hook 带入的依赖 → 生成导入文件 → 持久化结果。
	// Since Setup can be invoked in different contexts (i.e, via `otelc setup` or as part of `otelc go build`),
	// we need to handle the arguments accordingly. If the command is `go build` or `go install`, we should trim the first argument
	args := cmd.Args().Slice()
	subcommand := subcmdBuild
	if cmd.Name == "go" {
		subcommand = cmd.Args().First() // build / install / test
		args = cmd.Args().Tail()        // trim the subcommand
	}

	logger := util.LoggerFromContext(ctx)

	// Vendored projects fail the vendor consistency check: setup edits go.mod
	// for the injected hook modules but not vendor/modules.txt. Force module
	// mode so both build phases resolve dependency sources from the module cache
	// (matching versions and paths) instead of vendor/, leaving the user's
	// vendor directory untouched. This must run before isSetup() and
	// getBuildPackages() below so a cached-setup `otelc go build` still sets
	// GOFLAGS for the later buildWithToolexec, and before the findDeps dry run
	// further down. Computed here rather than threaded in because Setup is also
	// a standalone command action (otelc setup).
	vendored := vendoringActive(ctx, util.GetOtelcWorkDir())
	if vendored {
		logger.InfoContext(ctx, "vendored project detected; building with -mod=mod")
		// Mutates GOFLAGS process-wide with no restore. Fine for the one-shot
		// CLI; a second in-process GoBuild would inherit this -mod=mod.
		if err := os.Setenv("GOFLAGS", forceModMod(os.Getenv("GOFLAGS"))); err != nil {
			return ex.Wrapf(err, "forcing module mode for vendored build")
		}

		// A CLI -mod=vendor beats the GOFLAGS=-mod=mod forced above, so the Phase-1
		// build-plan dry run (both the auto-pin and the direct findDeps fallback
		// below call it) would still resolve vendor/ paths without an @version;
		// rewrite it to module mode when vendoring is active so both build phases
		// agree on where the dependency source lives.
		args = rewriteModVendor(subcommand, args)
	}

	if isSetup() {
		logger.InfoContext(ctx, "Setup has already been completed, skipping setup.")
		return nil
	}

	sp := &setupPhase{
		logger:     logger,
		ruleConfig: cmd.String("rules"),
		buildFlags: extractBuildFlags(subcommand, args),
	}

	// Introduce additional hook code by generating otelc.runtime.go
	// Use GetPackage to determine the build target directory
	buildPkgs, allPkgs, err := getBuildPackages(ctx, subcommand, args)
	if err != nil {
		return err
	}
	// Only the real packages are built. The test packages still feed the name
	// table, because they carry imports that only _test.go files use.
	sp.buildPackages = buildPkgs
	sp.resolvedNames = pkgload.CollectPackageNames(allPkgs)

	// Find the module directories for the build packages
	moduleDirs, findModErr := pkgload.FindModuleDirs(ctx, buildPkgs)
	if findModErr != nil {
		return findModErr
	}

	// Ensure a state manager is available in the context
	stateManager, found := stateManagerFromContext(ctx)
	if !found {
		// save this state manager in the context
		ctx = contextWithStateManager(ctx, stateManager)
	}

	// Auto-pin generates/updates otel.instrumentation.go file
	var deps []*Dependency
	// 自定义规则意味着用户已决定规则来源，跳过自动选择组件。
	// 默认路径下 autoPin 会先备份模块文件，再维护工具声明、组件导入及模块依赖。
	if sp.ruleConfig == "" && os.Getenv(util.EnvOtelcRules) == "" {
		pinResult, pinErr := autoPin(ctx, moduleDirs, subcommand, args)
		if pinErr != nil {
			return pinErr
		}
		deps = pinResult.AllDeps
	}

	if deps == nil {
		// Find all dependencies of the project being build
		deps, err = findDeps(ctx, subcommand, args)
		if err != nil {
			return err
		}
	}

	// Match the hook code with these dependencies
	matched, err := sp.matchDeps(ctx, deps, moduleDirs)
	if err != nil {
		return ex.Wrapf(err, "matching dependencies to hook rules")
	}

	// The hook packages the pass above selected get blank-imported into the
	// application below, which compiles their dependencies too. The build plan
	// never saw those, so match them now.
	injected, err := sp.matchInjectedDeps(
		ctx, matched, deps, moduleDirs, sp.buildFlags,
	)
	if err != nil {
		return err
	}
	matched = append(matched, injected...)

	// Reported here rather than per matching pass: an individual pass matching
	// nothing is normal, it is the combined result that tells the user whether
	// anything will be instrumented.
	if len(matched) == 0 {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: no instrumentation will be applied\n")
		sp.Warn("no instrumentation rules matched any dependencies")
	}

	// Generate otelc.runtime.go for all packages
	if err = sp.generateRuntimePerPackage(ctx, buildPkgs, matched); err != nil {
		return err
	}

	// matched.json 是两个阶段的进程间契约，不是完整原始规则列表：
	// 里面已带有匹配到的包/绝对源文件路径，以及 hook import path 解析出的本地目录。
	// 编译回调据此定位规则，避免每个编译子进程重新遍历全工程。
	// Write the matched ruleset to matched.json for further instrument phase
	return sp.store(ctx, matched, moduleDirs)
}

// setupGoCache creates a persistent GOCACHE in .otelc-build/gocache if one isn't already set.
// This prevents cache pollution when modifying core packages via //go:linkname while
// allowing incremental builds to work properly.
func setupGoCache(ctx context.Context, env []string) ([]string, error) {
	if os.Getenv("GOCACHE") != "" {
		// User has explicitly set GOCACHE, respect it
		return env, nil
	}

	logger := util.LoggerFromContext(ctx)
	cacheDir := util.GetBuildTemp("gocache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, ex.Wrapf(err, "failed to create persistent GOCACHE")
	}

	env = append(env, "GOCACHE="+cacheDir)
	logger.DebugContext(ctx, "using GOCACHE", "path", cacheDir)
	return env, nil
}

// extractBuildFlags extracts flags that affect the build context from the arguments.
// These flags need to be forwarded to `go list` when resolving import archives.
// Returns a slice of flag arguments preserving their original form.
//
// For boolean flags, the last occurrence wins. This correctly handles cases like:
//   - GOFLAGS=-race with -race=false on CLI (result: -race=false)
//   - -race -race=false (result: -race=false)
//   - -race=false -race (result: -race)
func extractBuildFlags(subcommand string, args []string) []string {
	// 包加载和后续归档解析必须使用与实际构建相同的 tags、race、overlay 等上下文；
	// 否则 setup 看见的文件、签名甚至依赖包可能与编译阶段不同。
	if subcommand == "" {
		subcommand = subcmdBuild
	}
	classified := classifyArgs(subcommand, args)
	var valueFlags []string
	type boolFlagValue struct {
		set   bool
		value bool
	}
	boolFlagState := make(map[string]boolFlagValue) // Track final state of boolean flags

	for i, a := range classified {
		if a.Kind != ArgBuildFlag {
			continue
		}

		if isBuildContextFlagWithValue(a.FlagName) {
			if a.HasValue {
				valueFlags = append(valueFlags, a.Raw)
			} else if i+1 < len(classified) && classified[i+1].Kind == ArgBuildFlagValue {
				valueFlags = append(valueFlags, a.Raw, classified[i+1].Raw)
			}
			continue
		}

		if isBuildContextBoolFlag(a.FlagName) {
			if a.HasValue {
				if enabled, err := strconv.ParseBool(a.Value); err == nil {
					boolFlagState[a.FlagName] = boolFlagValue{set: true, value: enabled} // Last value wins
				}
			} else {
				boolFlagState[a.FlagName] = boolFlagValue{set: true, value: true}
			}
			continue
		}
	}

	// Collect boolean flags that are enabled (in deterministic order)
	var enabledBoolFlags []string
	for flag, state := range boolFlagState {
		if state.set {
			if state.value {
				enabledBoolFlags = append(enabledBoolFlags, flag)
			} else {
				enabledBoolFlags = append(enabledBoolFlags, flag+"=false")
			}
		}
	}
	slices.Sort(enabledBoolFlags)

	return append(valueFlags, enabledBoolFlags...)
}

// toolexecInsertArg builds the -toolexec=... argument for execPath. Split out
// from buildWithToolexec so the error path (reachable only when execPath
// contains both quote characters) is testable without depending on
// os.Executable.
func toolexecInsertArg(execPath string) (string, error) {
	insert, err := util.BuildToolexecFlag(execPath)
	if err != nil {
		// BuildToolexecFlag's own error already names execPath, so there
		// is nothing to add here, matching nestedToolexecGoflagsToken
		// and the #1231 convention.
		return "", err
	}
	return insert, nil
}

// addBuildFlags inserts flags after a leading -C, which the go command requires
// to be the first flag on its command line.
func addBuildFlags(args []string, flags ...string) []string {
	insertAt := 0
	if len(args) > 0 {
		switch {
		case (args[0] == "-C" || args[0] == "--C") && len(args) > 1:
			insertAt = 2
		case strings.HasPrefix(args[0], "-C=") || strings.HasPrefix(args[0], "--C="):
			insertAt = 1
		}
	}

	result := make([]string, 0, len(args)+len(flags))
	result = append(result, args[:insertAt]...)
	result = append(result, flags...)
	return append(result, args[insertAt:]...)
}

// toolexecBuildArgs assembles the argv for the instrumented build: the
// original go subcommand, -work, the -toolexec flag pointing at execPath, and
// the caller's remaining arguments. Kept free of side effects so it can be
// tested directly; buildWithToolexec cannot be, since it would spawn a go
// build whose -toolexec target is the test binary itself.
func toolexecBuildArgs(args []string, execPath string, vendored bool) ([]string, error) {
	// -toolexec 把“启动 compile/link 等工具”的动作交给 otelc 包装，
	// -work 保留 Go 构建工作目录，供插桩阶段使用编译参数和调试生成结果。
	insert, err := toolexecInsertArg(execPath)
	if err != nil {
		return nil, err
	}
	const additionalCount = 3
	newArgs := make([]string, 0, len(args)+additionalCount) // Avoid in-place modification
	// Add "go build"
	newArgs = append(newArgs, "go")
	newArgs = append(newArgs, args[:1]...)
	// Add the rest
	subcommand := args[0]
	restArgs := args[1:]
	if vendored {
		restArgs = rewriteModVendor(subcommand, restArgs)
	}
	if _, fileTargets, err2 := splitBuildTargets(subcommand, restArgs); err2 == nil && len(fileTargets) > 0 {
		// add otelc.runtime.go manually to command line for file targets
		dir := filepath.Dir(fileTargets[0])
		otelcRuntimePath := filepath.Join(dir, otelcRuntimeFile)
		if util.PathExists(otelcRuntimePath) {
			// Keep the runtime file next to the file targets. In go test,
			// a file path after a flag is passed to the test binary instead.
			insertAt := lastFileTargetIndex(subcommand, restArgs) + 1
			restArgs = slices.Insert(restArgs, insertAt, otelcRuntimePath)
		}
	}
	// Add -work and -toolexec after a leading -C/--C. The go command rejects -C
	// when any other flag comes before it.
	restArgs = addBuildFlags(restArgs, "-work", insert)
	return append(newArgs, restArgs...), nil
}

// runBuildCmd runs the assembled go build. It is a variable so tests can
// replace it: the -toolexec target is os.Executable(), which under `go test`
// is the test binary, so really running the command would make the test
// binary its own toolexec target and re-invoke itself without bound.
//
//nolint:gochecknoglobals // test seam
var runBuildCmd = util.RunCmdWithEnv

// executablePath resolves the path to the current executable. It is a variable
// so tests can simulate paths that trigger quoting errors.
//
//nolint:gochecknoglobals // test seam
var executablePath = os.Executable

// buildWithToolexec builds the project with the toolexec mode. vendored is
// passed in by GoBuild: Setup already forced GOFLAGS=-mod=mod, but a CLI
// -mod=vendor beats GOFLAGS, so it still has to be neutralized in the build
// args and forwarded flags below.
func buildWithToolexec(ctx context.Context, cmd *cli.Command, vendored bool) error {
	args := cmd.Args().Slice()
	logger := util.LoggerFromContext(ctx)

	// Add -toolexec=otelc to the original build command and run it
	execPath, err := executablePath()
	if err != nil {
		return ex.Wrapf(err, "failed to get executable path")
	}
	newArgs, err := toolexecBuildArgs(args, execPath, vendored)
	if err != nil {
		return err
	}
	logger.InfoContext(ctx, "Running go build with toolexec", "args", newArgs)

	// Tell the sub-process the working directory
	env := os.Environ()
	pwd := util.GetOtelcWorkDir()
	util.Assert(pwd != "", "invalid working directory")
	env = append(env, fmt.Sprintf("%s=%s", util.EnvOtelcWorkDir, pwd))

	// Extract and forward build flags that affect the build context
	// This ensures `go list` resolves archives matching the current build
	subcommand := args[0]
	restArgs := args[1:]
	buildFlags := extractBuildFlags(subcommand, restArgs)
	if vendored {
		buildFlags = rewriteModVendor(subcommand, buildFlags)
	}
	if len(buildFlags) > 0 {
		encoded := util.EncodeBuildFlags(buildFlags)
		env = append(env, fmt.Sprintf("%s=%s", util.EnvOtelcBuildFlags, encoded))
		logger.DebugContext(ctx, "forwarding build flags", "flags", buildFlags)
	}

	// Use a fresh GOCACHE to prevent cache pollution when modifying core packages
	env, err = setupGoCache(ctx, env)
	if err != nil {
		return ex.Wrapf(err, "configuring go cache")
	}

	return runBuildCmd(ctx, env, newArgs...)
}

func GoBuild(ctx context.Context, cmd *cli.Command) error {
	// Validate the invocation before taking the build lock: a bad command
	// line must fail immediately, not wait behind a running build.
	if !cmd.Args().Present() {
		return ex.Newf("no command provided. Only 'go build', 'go install' and 'go test' are supported")
	}

	switch cmd.Args().First() {
	case subcmdBuild, subcmdInstall, subcmdTest:
		// supported
	default:
		return ex.Newf("unsupported command: %s. Only 'go build', 'go install' and 'go test' are supported",
			cmd.Args().First())
	}

	// Serialize with other otelc invocations in this module before touching
	// any shared state (tracking files, go.mod, .otelc-build contents).
	return withBuildLock(ctx, func(ctx context.Context) error {
		return runGoBuild(ctx, cmd)
	})
}

func runGoBuild(ctx context.Context, cmd *cli.Command) error {
	// 完整生命周期处于同一把构建锁之下，恢复也必须先于解锁发生。
	// 这和 Java 启动时 attach agent 不同：应用二进制生成之前，hook 已经参与编译与链接。
	ctx = contextWithStateManager(ctx, newStateManager())
	logger := util.LoggerFromContext(ctx)

	// Clean up import tracking files from previous builds at the start
	// to prevent stale data from affecting this build.
	instrument.CleanupImportTrackingFiles()

	// Go 的 defer 在函数返回时执行，因此 setup 或实际编译返回错误也会进入清理。
	// 进程被强杀时 defer 无法执行，stateManager 的磁盘清单提供后续手动恢复依据。
	defer func() {
		// Restore backed-up go.mod/go.sum but keep .otelc-build/ for debugging.
		// Users can run `otelc cleanup` to remove it explicitly. Deferred
		// inside the locked body so the restore always runs under the lock.
		if cleanErr := Cleanup(ctx, false); cleanErr != nil {
			logger.DebugContext(ctx, "cleanup failed", "error", cleanErr)
		}
	}()

	statsEnabled := os.Getenv(util.EnvOtelcStats) != ""

	// Setup forces GOFLAGS=-mod=mod for a vendored project (needed for both
	// entry points, otelc setup and otelc go build). vendored is still computed
	// here too so buildWithToolexec can rewrite an explicit CLI -mod=vendor,
	// which beats GOFLAGS.
	pwd := util.GetOtelcWorkDir()
	vendored := vendoringActive(ctx, pwd)

	setupStart := time.Now()
	err := Setup(ctx, cmd)
	if err != nil {
		return err
	}
	if statsEnabled {
		logger.InfoContext(ctx, "setup stats", "duration", time.Since(setupStart))
	}
	logger.InfoContext(ctx, "Setup completed successfully")

	buildStart := time.Now()
	err = buildWithToolexec(ctx, cmd, vendored)
	if err != nil {
		return err
	}
	if statsEnabled {
		logger.InfoContext(ctx, "build stats", "duration", time.Since(buildStart))
	}
	logger.InfoContext(ctx, "Instrumentation completed successfully")
	return nil
}
