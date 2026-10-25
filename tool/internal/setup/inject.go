// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 原始应用的编译计划只包含它本来依赖的包；导入 hook 后，依赖图会扩张。
// 本文件将新出现的包重新交给规则匹配，直到没有新包或新匹配，形成依赖闭包。
package setup

import (
	"context"
	"fmt"
	"os"
	"slices"

	"golang.org/x/tools/go/packages"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/internal/pkgload"
	"go.opentelemetry.io/otelc/tool/internal/rule"
)

// hookPackagePaths returns the packages otelc.runtime.go blank-imports for the
// given rules, which is every hook package those rules name.
func hookPackagePaths(matched []*rule.InstRuleSet) []string {
	// 函数规则与文件规则通过 Path 引入 hook 包。按 import path 去重，
	// 一个组件的多个函数命中不应反复加载相同 hook 依赖图。
	seen := make(map[string]bool)
	paths := make([]string, 0)
	add := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	for _, m := range matched {
		for _, r := range m.AllFuncRules() {
			add(r.Path)
		}
		for _, r := range m.FileRules {
			add(r.Path)
		}
	}
	return paths
}

// injectedDeps returns the packages the given hook packages bring into the
// build that known does not already cover, marking each one in known.
func injectedDeps(
	ctx context.Context,
	hookPaths []string,
	known map[string]bool,
	buildFlags []string,
) ([]*Dependency, error) {
	// 这里用 go/packages 遍历 hook 包的传递依赖；它补齐的是原 dry-run 尚未见到的包。
	// known 是调用方共享的集合，访问成功后立即标记，防止环形依赖/重复路径反复处理。
	if len(hookPaths) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, packagesLoadTimeout)
	defer cancel()

	pkgs, err := pkgload.LoadPackages(
		ctx,
		packages.NeedName|packages.NeedFiles|packages.NeedImports|packages.NeedDeps,
		buildFlags,
		false,
		hookPaths...,
	)
	if err != nil {
		return nil, ex.Wrapf(err, "loading hook packages to complete the build plan")
	}

	deps := make([]*Dependency, 0)
	var loadErr error
	// Visit 的第二个回调为前序访问，第三个为后序访问；这里仅使用后序回调。
	// Load 的顶层 error 为空不保证每个包均成功，故还要检查 pkg.Errors。
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		// packages.Load can return a nil top-level error while reporting
		// failures on individual packages in the closure. Walking past one
		// would match rules against a graph that is silently missing part of
		// what the hook actually imports.
		if len(pkg.Errors) > 0 && loadErr == nil {
			loadErr = ex.Newf("loading package %q: %v", pkg.PkgPath, pkg.Errors[0])
		}
		// A package with no Go files of its own has nothing to rewrite, and
		// Sources[0] below would panic.
		if pkg.PkgPath == "" || known[pkg.PkgPath] || len(pkg.GoFiles) == 0 {
			return
		}
		known[pkg.PkgPath] = true
		deps = append(deps, &Dependency{
			ImportPath: pkg.PkgPath,
			Version:    findModVersion(pkg.GoFiles[0]),
			Sources:    slices.Clone(pkg.GoFiles),
			// Known limitation: the plan path reads the cgo mapping out of the
			// compile commands (see findGoSources), which go/packages does not
			// expose here. A rule targeting a cgo package reached only through a
			// hook therefore matches without one. No such rule exists today.
			CgoFiles: map[string]string{},
		})
	})
	if loadErr != nil {
		return nil, loadErr
	}
	return deps, nil
}

// injectionPass is the pair of operations one pass performs. They are supplied
// by the caller so the loop can be exercised without loading or matching real
// packages.
type injectionPass struct {
	load  func(ctx context.Context, hookPaths []string, known map[string]bool) ([]*Dependency, error)
	match func(ctx context.Context, deps []*Dependency) ([]*rule.InstRuleSet, error)
}

// runInjectionPasses repeats load-then-match until a pass turns up nothing new.
func runInjectionPasses(
	ctx context.Context,
	matched []*rule.InstRuleSet,
	known map[string]bool,
	pass injectionPass,
) ([]*rule.InstRuleSet, error) {
	// 例如：应用命中规则 A → A 的 hook 引入包 B → B 命中规则 C → C 又引入包 D。
	// 每一轮只从“本轮新匹配”的 hook 出发，known 记录之前所有包，直到达到不动点。
	// 若出现新包却没有新规则，也可结束：这些包已遍历完，不会再引入新规则的 hook。
	extra := make([]*rule.InstRuleSet, 0)
	for {
		injected, err := pass.load(ctx, hookPackagePaths(matched), known)
		if err != nil {
			return nil, err
		}
		if len(injected) == 0 {
			return extra, nil
		}

		more, err := pass.match(ctx, injected)
		if err != nil {
			return nil, err
		}
		if len(more) == 0 {
			return extra, nil
		}

		extra = append(extra, more...)
		// Only a rule matched in this pass can name a hook package that has not
		// been walked yet, so the next pass starts from what this one found.
		matched = more
	}
}

// matchInjectedDeps matches rules against the packages that only enter the
// build because otelc put them there, and returns the extra rule sets.
//
// The build plan comes from a dry run of the application as written. Matching
// then decides which hook packages otelc.runtime.go will blank-import, and that
// import drags each hook package's own dependencies into the build. Those
// packages were never in the plan, so a rule targeting one of them has nothing
// to match against.
//
// Nothing reports this. The build succeeds, matched.json is a valid non-empty
// list, and the rule is simply absent from it.
//
// Instrumentation for a third-party library does not hit this, because the
// application imports the library being instrumented, so the plan already
// covers it. It bites a hook package whose rules target its own module, which
// the application has no reason to import.
func (sp *setupPhase) matchInjectedDeps(
	ctx context.Context,
	matched []*rule.InstRuleSet,
	deps []*Dependency,
	moduleDirs []string,
	buildFlags []string,
) ([]*rule.InstRuleSet, error) {
	// 这里刻意区分两类失败：补充图加载失败时输出警告并保留原始计划；
	// 已加载包的规则匹配失败则向上返回错误。前者可能导致新注入依赖漏插桩，
	// 因此看到构建成功时，仍应留意相应 Warning 与 matched.json。
	known := make(map[string]bool, len(deps))
	for _, dep := range deps {
		known[dep.ImportPath] = true
	}

	pass := injectionPass{
		load: func(ctx context.Context, hookPaths []string, known map[string]bool) ([]*Dependency, error) {
			injected, err := injectedDeps(ctx, hookPaths, known, buildFlags)
			if err != nil {
				// Completing the plan is an improvement on it, not a
				// precondition for building, so a closure that will not resolve
				// leaves the build running on the plan otelc already has. That
				// tradeoff is only honest if it is visible on the console: this
				// is the same silent miss the rest of this change fixes.
				_, _ = fmt.Fprintf(os.Stderr,
					"Warning: could not resolve the packages otelc injects into the build; "+
						"rules targeting them will not be applied: %v\n", err)
				sp.Warn("could not resolve the packages otelc injects into the build; "+
					"rules targeting them will not be applied", "error", err)
				return nil, nil
			}
			if len(injected) > 0 {
				sp.Info("Matching dependencies otelc injects into the build", "count", len(injected))
			}
			return injected, nil
		},
		match: func(ctx context.Context, injected []*Dependency) ([]*rule.InstRuleSet, error) {
			more, err := sp.matchDeps(ctx, injected, moduleDirs)
			if err != nil {
				return nil, ex.Wrapf(err, "matching injected dependencies to hook rules")
			}
			return more, nil
		},
	}

	extra, err := runInjectionPasses(ctx, matched, known, pass)
	if err != nil {
		return nil, err
	}
	return extra, nil
}
