// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package instrument

import (
	"context"
	_ "embed"
	"fmt"
	"go/parser"
	"path/filepath"
	"strings"

	"github.com/dave/dst"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/internal/ast"
	"go.opentelemetry.io/otelc/tool/internal/rule"
	"go.opentelemetry.io/otelc/tool/util"
)

const (
	tJumpLabel       = "/* __TRAMPOLINE_JUMP_IF__ */"
	otelcGlobalsFile = "otelc.globals.go"
)

func makeName(r *rule.InstFuncRule, funcDecl *dst.FuncDecl, isBefore bool) string {
	prefix := trampolineAfterName
	if isBefore {
		prefix = trampolineBeforeName
	}
	return fmt.Sprintf("%s_%s%s",
		prefix, funcDecl.Name.Name, r.Identity())
}

func findJumpPoint(jumpIf *dst.IfStmt) *dst.BlockStmt {
	// Multiple func rules may apply to the same function, we need to find the
	// appropriate jump point to insert trampoline jump.
	if len(jumpIf.Decs.If) == 1 && jumpIf.Decs.If[0] == tJumpLabel {
		// Insert trampoline jump within the else block
		elseBlock := util.AssertType[*dst.BlockStmt](jumpIf.Else)
		if len(elseBlock.List) > 1 {
			// One trampoline jump already exists, recursively find last one
			ifStmt := util.AssertType[*dst.IfStmt](elseBlock.List[len(elseBlock.List)-1])
			return findJumpPoint(ifStmt)
		}
		// Otherwise, this is the appropriate jump point
		return elseBlock
	}
	return nil
}

// syntheticNamer returns a generator of "prefix_hash_N" identifiers. hash is
// a per-rule value (see InstFuncRule.Identity) folded into every generated
// name to prevent conflicts with other identifiers.
// The signature (receiver, parameters, and results) is still checked defensively
// so the generator never reuses a name that is already declared there or one
// it has already generated.
func syntheticNamer(funcDecl *dst.FuncDecl, hash string) func(prefix string) string {
	taken := make(map[string]bool)
	for _, list := range []*dst.FieldList{funcDecl.Recv, funcDecl.Type.Params, funcDecl.Type.Results} {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, name := range field.Names {
				taken[name.Name] = true
			}
		}
	}
	idx := 0
	return func(prefix string) string {
		for {
			name := fmt.Sprintf("%s_%s_%d", prefix, hash, idx)
			idx++
			if !taken[name] {
				taken[name] = true
				return name
			}
		}
	}
}

func collectReturnValues(funcDecl *dst.FuncDecl, hash string) []string {
	// 中文导读：After 必须拿到“最终会返回的那组变量”的地址，所以把匿名结果
	// 命名，把 _ 换成唯一名字。Go 的 return expr 会先写入具名结果，再执行
	// defer；After 因而能读取结果，也能在函数真正返回前通过地址改写它。
	// 这里改的是签名 AST，不是运行时通过反射遍历返回值。
	// Add explicit names for return values, they can be further referenced if
	// we're willing
	var retVals []string // nil by default
	if retList := funcDecl.Type.Results; retList != nil {
		next := syntheticNamer(funcDecl, hash)
		for _, field := range retList.List {
			util.Assert(field.Type != nil, "why not otherwise")
			if field.Names == nil {
				// Unnamed Return Values, e.g. func() (int, string)
				// Rename (for referenceability)
				name := next(unnamedRetValName)
				field.Names = []*dst.Ident{ast.Ident(name)}
				// Collect (for further use)
				retVals = append(retVals, name)
			} else {
				// Named Return Values, e.g. func() (a int, b string)
				// Collect only (for further use)
				for _, name := range field.Names {
					if name.Name == ast.IdentIgnore {
						name.Name = next(ignoredRetValName)
					}
					retVals = append(retVals, name.Name)
				}
			}
		}
	}

	return retVals
}

func collectArguments(funcDecl *dst.FuncDecl, hash string) []string {
	// 中文导读：接收者也算 hook 参数，且固定排在索引 0；普通函数则从第一个
	// 显式参数开始。例：func (c *Client) Do(ctx context.Context) 中，
	// GetParam(0) 对应 c、GetParam(1) 对应 ctx。匿名参数和 _ 必须先命名，
	// 因为后续生成 &参数，而 Go 不允许取空白标识符 _ 的地址。
	args := make([]string, 0)
	next := syntheticNamer(funcDecl, hash)
	if ast.HasReceiver(funcDecl) {
		if recv := funcDecl.Recv.List[0]; recv.Names != nil {
			// Named receiver, e.g. func (r R) F() {} or func (_ R) F() {}
			receiver := recv.Names[0].Name
			if receiver == ast.IdentIgnore {
				// Blank receiver: recv.Names holds "_" rather than being nil,
				// so it falls into this branch, but "_" cannot have its
				// address taken during trampoline generation the way a named
				// receiver can. Assign it a generated name instead.
				receiver = next(ignoredParam)
				recv.Names[0].Name = receiver
			}
			args = append(args, receiver)
		} else {
			// Unnamed receiver, e.g. func (R) F() {}
			receiver := next(ignoredParam)
			funcDecl.Recv.List[0].Names = []*dst.Ident{ast.Ident(receiver)}
			args = append(args, receiver)
		}
	}
	for _, field := range funcDecl.Type.Params.List {
		util.Assert(field.Type != nil, "why not otherwise")
		if field.Names == nil {
			// Unnamed Parameters, e.g. func(int, string){}
			// Assign a name for these parameters and collect it then
			name := next(ignoredParam)
			field.Names = []*dst.Ident{ast.Ident(name)}
			args = append(args, name)
		} else {
			// Named Parameters, e.g. func(a int, b string){}
			for _, name := range field.Names {
				if name.Name == ast.IdentIgnore {
					name.Name = next(ignoredParam)
				}
				args = append(args, name.Name)
			}
		}
	}
	return args
}

func createTrampArgs(names []string) []dst.Expr {
	// 中文导读：传递的是原函数局部参数/结果变量的地址，而非简单复制其值。
	// 若原类型已经是 *Client，这里得到 **Client，可替换该局部指针本身；
	// 这与仅修改 Client 指向对象的字段是两种不同的操作。
	exprs := make([]dst.Expr, 0, len(names))
	for _, name := range names {
		util.Assert(name != ast.IdentIgnore, "must be processed before")
		exprs = append(exprs, ast.AddressOf(name))
	}
	return exprs
}

func createTJumpIf(file *dst.File, t *rule.InstFuncRule, funcDecl *dst.FuncDecl,
	args, retVals []string,
) *dst.IfStmt {
	// 中文导读：下面构造的 AST 会在“应用运行时”执行。Before 返回的 skip
	// 为 true 时，先调用 After，再直接返回具名结果，原函数体完全不执行；
	// 否则注册 defer After 后继续原函数体。defer 注册时就保存结果变量地址，
	// 所以调用 After 时读到的是稍后写入的结果，而非注册 defer 时的零值。
	// 原函数 panic 展开栈时也会执行已注册的 After；trampoline 对自身 hook
	// panic 的 recover 不会因此吞掉原函数的 panic，也没有把该 panic 作为参数传入。
	funcSuffix := t.Identity()
	argsToBefore := createTrampArgs(args)
	argsToAfter := createTrampArgs(retVals)
	argHookContext := ast.Ident(trampolineHookContextName + funcSuffix)
	argsToAfter = append([]dst.Expr{argHookContext}, argsToAfter...)
	beforeCallName := makeName(t, funcDecl, true)
	afterCallName := makeName(t, funcDecl, false)
	typeParams := findTargetGenericType(file, funcDecl)
	beforeCall := ast.CallTo(beforeCallName, typeParams, argsToBefore)
	afterCall := ast.CallTo(afterCallName, typeParams, argsToAfter)
	tjumpInit := ast.DefineStmts(
		ast.Exprs(
			ast.Ident(trampolineHookContextName+funcSuffix),
			ast.Ident(trampolineSkipName+funcSuffix),
		),
		ast.Exprs(beforeCall),
	)
	tjumpCond := ast.Ident(trampolineSkipName + funcSuffix)
	tjumpReturn := make([]dst.Expr, 0, len(retVals))
	for _, retVal := range retVals {
		tjumpReturn = append(tjumpReturn, ast.Ident(retVal))
	}
	tjumpBody := ast.BlockStmts(
		ast.ExprStmt(afterCall),
		ast.ReturnStmt(tjumpReturn),
	)
	tjumpElse := ast.Block(ast.DeferStmt(afterCall))
	tjump := ast.IfStmt(tjumpInit, tjumpCond, tjumpBody, tjumpElse)
	tjump.Decs.If.Append(tJumpLabel)
	return tjump
}

func (ip *instrumentPhase) insertToFunc(funcDecl *dst.FuncDecl, tjump *dst.IfStmt) {
	// 中文导读：同一目标可匹配多条 func 规则，新的入口跳转嵌入前一条的
	// else 中；只有前面的规则允许继续，才会进入后面的规则。已经注册的
	// After 遵守 Go defer 的后进先出顺序，形成类似嵌套拦截器的退出顺序。
	// 首次插入还会生成 line 指令，将注入部分标为 generated，再把原语句
	// 映射回原文件行号，让调试器和错误堆栈尽量保留用户熟悉的源码位置。
	found := false
	if len(funcDecl.Body.List) > 0 {
		firstStmt := funcDecl.Body.List[0]
		if ifStmt, ok := firstStmt.(*dst.IfStmt); ok {
			point := findJumpPoint(ifStmt)
			if point != nil {
				point.List = append(point.List, ast.EmptyStmt())
				point.List = append(point.List, tjump)
				found = true
			}
		}
	}
	if !found {
		// Tag the trampoline-jump-if with a special line directive so that
		// debugger can show the correct line number
		tjump.Decs.Before = dst.NewLine
		tjump.Decs.Start.Append("//line <generated>:1")
		pos := ip.parser.FindPosition(funcDecl.Body)
		if len(funcDecl.Body.List) > 0 {
			// It does happens because we may insert raw code snippets at the
			// function entry. These dynamically generated nodes do not have
			// corresponding node positions. We need to keep looking downward
			// until we find a node that contains position information, and then
			// annotate it with a line directive.
			for _, stmt := range funcDecl.Body.List {
				pos = ip.parser.FindPosition(stmt)
				if !pos.IsValid() {
					continue
				}
				tag := fmt.Sprintf("//line %s", pos.String())
				stmt.Decorations().Before = dst.NewLine
				stmt.Decorations().Start.Append(tag)
			}
		} else {
			tag := fmt.Sprintf("//line %s", pos.String())
			empty := ast.EmptyStmt()
			empty.Decs.Before = dst.NewLine
			empty.Decs.Start.Append(tag)
			funcDecl.Body.List = append(funcDecl.Body.List, empty)
		}
		funcDecl.Body.List = append([]dst.Stmt{tjump}, funcDecl.Body.List...)
	}
}

func (ip *instrumentPhase) insertTJump(t *rule.InstFuncRule, funcDecl *dst.FuncDecl) error {
	util.Assert(funcDecl.Name.Name == t.Func, "sanity check")

	// Record the target function for the whole trampoline creation process
	ip.targetFunc = funcDecl

	// Collect return values from target function
	retVals := collectReturnValues(funcDecl, t.Identity())

	// Collect all arguments from target function, including the receiver
	args := collectArguments(funcDecl, t.Identity())

	// Generate the trampoline-jump-if. The trampoline-jump-if is a conditional
	// jump that jumps to the trampoline function, it looks something like this
	//
	//	if ctx, skip := otel_trampoline_before(&arg); skip {
	//	    otel_trampoline_after(ctx, &retval)
	//	    return ...
	//	} else {
	//	    defer otel_trampoline_after(ctx, &retval)
	//	    ...
	//	}
	//
	// The trampoline function is just a relay station that properly assembles
	// the context, handles exceptions, etc, and ultimately jumps to the real
	// hook code. By inserting trampoline-jump-if at the target function entry,
	// we can intercept the original function and execute before/after hooks.
	tjump := createTJumpIf(ip.target, t, funcDecl, args, retVals)

	// Record the trampoline-jump-if as they can be optimized later, they are
	// performance-critical
	ip.tjumps = append(ip.tjumps, &tJump{target: funcDecl, ifStmt: tjump, rule: t})

	// Find if there is already a trampoline-jump-if, insert new tjump if so,
	// otherwise prepend to block body.
	ip.insertToFunc(funcDecl, tjump)

	// Trampoline-jump-if ultimately jumps to the trampoline function, which
	// typically has the following form
	//
	//	func otel_trampoline_before(arg) (HookContext, bool) {
	//	    defer func () { /* handle panic */ }()
	//	    // prepare hook context for real hook code
	//	    hookctx := &HookContextImpl_abc{}
	//	    ...
	//	    // Call the real hook code
	//		realHook(ctx, arg)
	//	    return ctx, skip
	//	}
	//
	// It catches any potential panic from the real hook code, and prepare the
	// hook context for the real hook code. Once all preparations are done, it
	// jumps to the real hook code. Note that each trampoline has its own hook
	// context implementation, which is generated dynamically.
	return ip.createTrampoline(t)
}

func (ip *instrumentPhase) addCompileArg(newArg string) {
	ip.compileArgs = append(ip.compileArgs, newArg)
}

//go:embed api.tmpl
var templateAPI string

func (ip *instrumentPhase) writeGlobals(pkgName string) error {
	// 中文导读：把共享 HookContext 接口和模板级变量写成独立的包内 Go 文件，
	// 并加到 compile 参数。各条规则专用的 HookContextImpl 与 trampoline
	// 则追加在改写的目标文件中；这个 globals 文件不是一个额外的运行时包。
	// Prepare trampoline code header
	p := ast.NewAstParser()
	trampoline, err := p.ParseSource("package " + pkgName)
	if err != nil {
		return ex.Wrapf(err, "parsing globals header for package %s", pkgName)
	}
	// Declare common variable declarations
	trampoline.Decls = append(trampoline.Decls, ip.varDecls...)

	// Declare the hook context interface
	api, err := p.ParseSource(templateAPI)
	if err != nil {
		return ex.Wrapf(err, "parsing api template")
	}
	trampoline.Decls = append(trampoline.Decls, api.Decls...)

	// Write trampoline code to file
	path := filepath.Join(ip.workDir, otelcGlobalsFile)
	err = ast.WriteFile(path, trampoline)
	if err != nil {
		return ex.Wrapf(err, "writing globals file %s", path)
	}
	ip.addCompileArg(path)
	ip.keepForDebug(path)
	return nil
}

func (ip *instrumentPhase) writeInstrumented(root *dst.File, oldFile string) error {
	// 中文导读：常规源码写到 Go 构建工作目录，compile 改用该路径，用户文件
	// 本身不因这次 AST 改写而保存。cgo 文件通常已在同一工作目录，可能原位
	// 写回，因此先复制出 vet 专用版本。keepForDebug 另留调试副本便于对照。
	// Write the instrumented AST to the new file in the working directory
	newFile := filepath.Join(ip.workDir, filepath.Base(oldFile))
	if strings.HasSuffix(oldFile, ".cgo1.go") {
		// The vet configuration refers to cgo's generated source. Preserve that
		// source because vet must not analyze otelc's injected declarations.
		vetFile := cgoVetSourcePath(oldFile)
		if err := util.CopyFile(oldFile, vetFile); err != nil {
			return ex.Wrapf(err, "preserving cgo source for vet %s", oldFile)
		}
	}
	err := ast.WriteFile(newFile, root)
	if err != nil {
		return ex.Wrapf(err, "writing instrumented file %s", newFile)
	}
	ip.keepForDebug(newFile)

	// Replace the original file with the new file in the compile command
	replace := false
	for i, arg := range ip.compileArgs {
		// Files in the compile command maybe relative or absolute, we need to
		// consolidate them to absolute path
		abs, err1 := filepath.Abs(arg)
		if err1 != nil {
			return ex.Wrap(err1)
		}
		if abs == oldFile {
			ip.compileArgs[i] = newFile
			replace = true
			break
		}
	}
	if !replace {
		return ex.Newf("cannot replace %s with %s during %v",
			oldFile, newFile, ip.compileArgs)
	}
	ip.Info("Write instrumented AST", "old", oldFile, "new", newFile)
	return nil
}

func (ip *instrumentPhase) parseFile(file string) (*dst.File, error) {
	ip.parser = ast.NewAstParser()
	root, err := ip.parser.Parse(file, parser.ParseComments)
	if err != nil {
		return nil, ex.Wrapf(err, "parsing source file %s", file)
	}
	ip.target = root
	// Every time we parse a file, we need to reset the trampoline jumps
	// because they are associated with one certain file
	ip.tjumps = make([]*tJump, 0)
	return root, nil
}

// applyFuncRule instruments the function targeted by rule and reports whether a
// trampoline was actually inserted (true) or the rule was skipped (false). The
// bool lets the caller avoid writing an otelc.globals.go file for a package
// whose only func rules were skipped via //otelc:ignore.
func (ip *instrumentPhase) applyFuncRule(ctx context.Context, rule *rule.InstFuncRule, root *dst.File,
	funcDecl *dst.FuncDecl, found bool,
) (bool, error) {
	// 中文导读：组件作者提供目标签名和 Before/After，框架负责把规则变成
	// trampoline。Identity 用于生成名字并在包内去重，不只是日志中的规则名；
	// 导入仍先处理，避免两条等价规则声明不同 imports 时漏掉所需依赖。
	if !found {
		return false, ex.Newf("can not find function %s", rule.Func)
	}
	var err error
	// A function-level //otelc:ignore opts this function out even when a rule
	// matches it. The func rule then contributes no instrumented function, so it
	// must not force a globals file to be written for the package.
	if ast.FuncLeadHasDirective(funcDecl, util.DirectiveIgnore) {
		ip.Debug("Skip func rule due to //otelc:ignore", "func", rule.Func, "rule", rule.Name)
		return false, nil
	}

	// Apply imports for every matching rule, including ones de-duplicated below:
	// two rules with the same content identity may still declare different
	// imports, and skipping them could drop an import the hook code needs.
	if err = ip.addRuleImports(ctx, root, usedRuleImports(root, rule.Imports, nil), rule.Name); err != nil {
		return false, err
	}

	// Skip package runtime itself to avoid a self-import.
	if ip.buildUsesIgnoreDirective && !ip.isSuppressHooksPackage() {
		suppressImport := map[string]string{suppressHooksPackage: suppressHooksPackage}
		if err = ip.addRuleImports(ctx, root, suppressImport, rule.Name); err != nil {
			return false, err
		}
	}

	// De-duplicate trampoline/HookContext emission for rules that resolve to the
	// same content identity: emitting again would redeclare byte-identical
	// declarations. Distinct do-sequence modifiers differ by content or by
	// application index, so this only collapses genuinely duplicate rules.
	id := rule.Identity()
	if _, seen := ip.appliedFuncIdentities[id]; seen {
		ip.Debug("Skipping duplicate func rule trampoline (imports already applied)",
			"rule", rule.Name, "func", rule.Func)
		// The identical rule already inserted a trampoline for this function, so
		// the function is instrumented and the globals file is still required.
		return true, nil
	}

	if err = ip.insertTJump(rule, funcDecl); err != nil {
		return false, err
	}
	if ip.appliedFuncIdentities == nil {
		ip.appliedFuncIdentities = make(map[string]struct{})
	}
	ip.appliedFuncIdentities[id] = struct{}{}
	ip.Info("Apply func rule", "rule", rule)
	return true, nil
}
