// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 工具配置文件用于声明“这个模块选择哪些插桩组件”，具体规则仍由组件目录提供。
// 典型文件名是 otel.instrumentation.go（也兼容 otelc.tool.go），其空白导入
// 被本文件解析为组件图；它与 add.go 生成的运行时导入文件承担不同职责。
// pin 生成的清单带 tools 构建标签，普通构建通常不编译它；setup 直接解析它的导入。
// go.mod 的 tool 声明则用于固定 otelc 工具本身，两者不要与业务运行时导入混为一谈。
package setup

import (
	"context"
	"errors"
	"go/parser"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"

	"github.com/dave/dst"
	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/internal/ast"
	"go.opentelemetry.io/otelc/tool/internal/pkgload"
	"go.opentelemetry.io/otelc/tool/util"
)

const (
	// Allowed names for the instrumentation config file.
	toolFileCanonical = "otel.instrumentation.go"
	toolFileAlias     = "otelc.tool.go"
)

// instrumentationConfig 是解析某个组件 import path 的结果。
// ToolFile 位于组件所属模块根目录，用于继续寻找组件；RuleFiles 位于该包目录树下。
// Error 保存在单个结果中，让调用方按使用场景决定报错、剪枝还是继续访问。
type instrumentationConfig struct {
	ImportPath string
	ToolFile   string
	RuleFiles  []string
	Error      error
}

//nolint:forbidigo // sentinel error; must not carry mutable stack state
var errNotInstrumentation = errors.New("not an instrumentation package")

func findToolFile(moduleDir string) (string, error) {
	// 两个文件名是兼容关系，而非可合并的两份配置；同时存在应报错，避免来源歧义。
	canonical := filepath.Join(moduleDir, toolFileCanonical)
	alias := filepath.Join(moduleDir, toolFileAlias)

	canonicalExists := util.PathExists(canonical)
	aliasExists := util.PathExists(alias)

	switch {
	case canonicalExists && aliasExists:
		return "", ex.Newf(
			"both %q and %q exist; only one instrumentation config file is allowed",
			toolFileCanonical,
			toolFileAlias,
		)
	case canonicalExists:
		return canonical, nil
	case aliasExists:
		return alias, nil
	default:
		return "", nil
	}
}

func findToolFiles(moduleDirs []string) ([]string, error) {
	toolFiles := make([]string, 0, len(moduleDirs))
	for _, dir := range moduleDirs {
		toolFile, err := findToolFile(dir)
		if err != nil {
			return nil, err
		}
		if toolFile != "" {
			toolFiles = append(toolFiles, toolFile)
		}
	}
	// Sort for deterministic rule loading.
	slices.Sort(toolFiles)
	return toolFiles, nil
}

const packagesLoadTimeout = 30 * time.Second

// resolveInstrumentationConfigs resolves instrumentation configs for the given import paths.
//
//nolint:nilnil // nil, nil when no imports are specified
func resolveInstrumentationConfigs(
	ctx context.Context,
	dir string,
	importPaths []string,
) (map[string]*instrumentationConfig, error) {
	// 用 Go 自己的模块解析定位源码目录，因而能遵守 require/replace 与模块缓存规则。
	// Dir 指向引用者目录：同一个 import path 必须在引用它的模块上下文中解析。
	// 此处只读元数据和目录，不要求工具专用包能够按普通构建标签成功编译。
	if len(importPaths) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, packagesLoadTimeout)
	defer cancel()

	pkgs, loadErr := packages.Load(&packages.Config{
		Mode:    packages.NeedFiles | packages.NeedModule | packages.NeedName,
		Context: ctx,
		Dir:     dir,
	}, importPaths...)
	if loadErr != nil {
		return nil, ex.Wrapf(loadErr, "failed to load instrumentation packages")
	}

	cfgs := make(map[string]*instrumentationConfig, len(importPaths))
	for _, pkg := range pkgs {
		importPath := pkg.PkgPath
		if importPath == "" {
			continue
		}

		cfgs[importPath] = &instrumentationConfig{
			ImportPath: importPath,
		}

		if pkg.Module == nil || pkg.Module.Dir == "" {
			cfgs[importPath].Error = ex.Newf("package %s is not part of a module", importPath)
			continue
		}

		modDir := pkg.Module.Dir
		// Prefer the directory of a real Go file. A tools-only instrumentation
		// package (its only .go file carries //go:build tools) has no buildable Go
		// files, so packages.Load reports "build constraints exclude all Go files"
		// and GoFiles is empty; fall back to deriving the dir from the module path +
		// import-path suffix (correct even under replace directives). The walk only
		// needs the directory, not a compilable package.
		pkgDir := pkgload.PackageDir(pkg)
		if pkgDir == "" {
			pkgDir = filepath.Join(modDir, filepath.FromSlash(strings.TrimPrefix(importPath, pkg.Module.Path)))
		}

		// Always look for tool file in the module directory
		toolFile, findErr := findToolFile(modDir)
		if findErr != nil {
			cfgs[importPath].Error = ex.Wrapf(
				findErr,
				"checking for tool file in instrumentation package %s",
				importPath,
			)
			continue
		}

		// 跳过嵌套 go.mod 所定义的子模块，避免一个组件意外吞入另一个模块的规则。
		ruleFiles, walkErr := rulesFromDir(pkgDir, true)
		if walkErr != nil {
			cfgs[importPath].Error = ex.Wrapf(walkErr, "walking instrumentation package %s", importPath)
			continue
		}

		if toolFile == "" && len(ruleFiles) == 0 {
			err := ex.Wrapf(
				errNotInstrumentation,
				"instrumentation package %s contains neither %s nor any rule files",
				importPath,
				toolFileCanonical,
			)

			if len(pkg.Errors) > 0 {
				var msgs []string
				for _, e := range pkg.Errors {
					msgs = append(msgs, e.Error())
				}
				err = ex.Wrapf(err, "package load errors: %s", strings.Join(msgs, "; "))
			}

			cfgs[importPath].Error = err
			continue
		}

		cfgs[importPath].ToolFile = toolFile
		cfgs[importPath].RuleFiles = ruleFiles
	}

	return cfgs, nil
}

type instrumentationVisit struct {
	Config   *instrumentationConfig
	ToolFile string
}

type instrumentationVisitor func(visit *instrumentationVisit) (recurse bool, err error)

func collectImports(toolFile string, f *dst.File, seenImports map[string]bool) ([]string, error) {
	// 工具配置是组件选择清单，所以只接受 `_ "组件路径"`。
	// imp.Path.Value 是带引号的 Go 字符串字面量；Unquote 后才是可交给包加载器的路径。
	importPaths := make([]string, 0, len(f.Imports))

	for _, imp := range f.Imports {
		if imp.Name == nil {
			return nil, ex.Newf(
				"%s: import %s must be a blank import (use `_ %s`)",
				toolFile,
				imp.Path.Value,
				imp.Path.Value,
			)
		}
		if imp.Name.Name != ast.IdentIgnore {
			return nil, ex.Newf(
				"%s: import %s must be a blank import (named imports are not allowed)",
				toolFile,
				imp.Path.Value,
			)
		}

		importPath, unquoteErr := strconv.Unquote(imp.Path.Value)
		if unquoteErr != nil {
			return nil, ex.Wrapf(
				unquoteErr,
				"failed to unquote import path %s in %s",
				imp.Path.Value,
				toolFile,
			)
		}

		if seenImports[importPath] {
			continue
		}

		seenImports[importPath] = true
		importPaths = append(importPaths, importPath)
	}

	return importPaths, nil
}

// walkInstrumentation walks the instrumentation tool files and calls the visitor function for each.
func walkInstrumentation(ctx context.Context, toolFiles []string, visit instrumentationVisitor) error {
	// 用队列做广度优先遍历，两个 seen 集合分别防止重复 import 和重复工具文件。
	// visitor 返回 recurse 控制是否追踪该组件模块的工具文件；不同使用者可实现
	// 规则收集或 pin 校验/剪枝，而不复制整套遍历逻辑。
	// 这里只解析 ImportsOnly，不执行文件中的 Go 代码，也不执行组件初始化函数。
	queue := append([]string(nil), toolFiles...)
	seenImports := make(map[string]bool)
	seenToolFiles := make(map[string]bool, len(toolFiles))
	for _, toolFile := range toolFiles {
		seenToolFiles[toolFile] = true
	}

	p := ast.NewAstParser()
	for len(queue) > 0 {
		toolFile := queue[0]
		queue = queue[1:]

		f, parseErr := p.Parse(toolFile, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}

		importPaths, collectErr := collectImports(toolFile, f, seenImports)
		if collectErr != nil {
			return collectErr
		}

		cfgs, resolveErr := resolveInstrumentationConfigs(ctx, filepath.Dir(toolFile), importPaths)
		if resolveErr != nil {
			return resolveErr
		}

		for _, importPath := range importPaths {
			cfg := cfgs[importPath]
			if cfg == nil {
				continue
			}

			v := &instrumentationVisit{
				Config:   cfg,
				ToolFile: toolFile,
			}
			recurse, visitErr := visit(v)
			if visitErr != nil {
				return visitErr
			}

			if recurse && cfg.ToolFile != "" {
				// Two different import paths may share the same tool file, so we need to de-duplicate it.
				if seenToolFiles[cfg.ToolFile] {
					continue
				}
				seenToolFiles[cfg.ToolFile] = true
				queue = append(queue, cfg.ToolFile)
			}
		}
	}

	return nil
}
