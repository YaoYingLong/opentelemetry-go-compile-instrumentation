// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// rule 包定义 YAML 配置进入工具后的数据模型，不负责执行观测逻辑。
// 原始文件经解析与 Normalize 转成具体规则；setup 将命中的规则按包/文件归组，
// instrument 再通过同一模型实施 AST 改写。新增组件通常复用现有模型而无需修改它。
package rule

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.opentelemetry.io/otelc/tool/util"
)

// InstRule defines the interface for an instrumentation rule. Each rule
// specifies a target module and version, and has a unique name. The version
// range is optional and is used to filter rules that are applicable to the
// target module version. If the version is not specified, the rule is applicable
// to all versions of the target module. The left bound is inclusive, the right
// bound is exclusive. For example, "v1.0.0,v2.0.0" means the rule is applicable
// to the target module version range [v1.0.0, v2.0.0).
// 中文导读：Go 接口通过方法集合隐式实现，无需写 implements。
// 各具体规则嵌入 InstBaseRule 后获得这些公共方法，因此能作为 InstRule 统一加载，
// 精确匹配时再用 type switch 区分函数、结构体、调用点等实际规则类型。
type InstRule interface {
	String() string      // The string representation of the rule
	GetName() string     // The unique name of the rule
	GetTarget() *Target  // The packages the rule applies to
	GetVersion() string  // The version range of target module if available, e.g "v1.0.0,v2.0.0"
	GetWhere() *WhereDef // Optional non-package selectors that remain after normalization
}

// FilterDef describes file predicates nested under where.file.
//
// The file predicate model currently supports implicit all-of across top-level
// fields plus the explicit qualifier keys needed by the agreed surface. The
// setup phase compiles leaf predicates and all-of/one-of/not combinations into
// executable filters.
// 中文导读：当前 setup/filter.go 已支持 all-of/one-of/not 的递归执行，
// 分别表示全部满足、至少一个满足和取反。扩展字段时仍需同时维护解析与执行。
// IsTest 用 *bool 区分“不限制测试属性”与“明确要求非测试”。
type FilterDef struct {
	AllOf        []FilterDef `json:"all-of,omitempty"        yaml:"all-of,omitempty"`
	OneOf        []FilterDef `json:"one-of,omitempty"        yaml:"one-of,omitempty"`
	Not          *FilterDef  `json:"not,omitempty"           yaml:"not,omitempty"`
	HasFunc      string      `json:"has_func,omitempty"      yaml:"has_func,omitempty"`
	HasRecv      string      `json:"has_recv,omitempty"      yaml:"has_recv,omitempty"`
	HasDirective string      `json:"has_directive,omitempty" yaml:"has_directive,omitempty"`
	HasStruct    string      `json:"has_struct,omitempty"    yaml:"has_struct,omitempty"`
	HasPackage   string      `json:"has_package,omitempty"   yaml:"has_package,omitempty"`
	IsTest       *bool       `json:"is_test,omitempty"       yaml:"is_test,omitempty"`
}

// WhereDef carries the structured where clause after package selectors have
// been split back out to top-level target/version fields.
//
// Today the setup phase only executes where.file predicates. The remaining
// selector and qualifier fields are preserved here so the agreed syntax surface
// can be normalized now without forcing the broader internal refactor yet.
type WhereDef struct {
	File          *FilterDef `json:"file,omitempty"           yaml:"file,omitempty"`
	AllOf         []WhereDef `json:"all-of,omitempty"         yaml:"all-of,omitempty"`
	OneOf         []WhereDef `json:"one-of,omitempty"         yaml:"one-of,omitempty"`
	Not           *WhereDef  `json:"not,omitempty"            yaml:"not,omitempty"`
	Func          string     `json:"func,omitempty"           yaml:"func,omitempty"`
	Recv          string     `json:"recv,omitempty"           yaml:"recv,omitempty"`
	Struct        string     `json:"struct,omitempty"         yaml:"struct,omitempty"`
	StructLiteral string     `json:"struct_literal,omitempty" yaml:"struct_literal,omitempty"`
	FunctionCall  string     `json:"function_call,omitempty"  yaml:"function_call,omitempty"`
	MethodCall    string     `json:"method_call,omitempty"    yaml:"method_call,omitempty"`
	Directive     string     `json:"directive,omitempty"      yaml:"directive,omitempty"`
	Kind          string     `json:"kind,omitempty"           yaml:"kind,omitempty"`
	Identifier    string     `json:"identifier,omitempty"     yaml:"identifier,omitempty"`
}

// InstBaseRule is the base rule for all instrumentation rules.
// 中文导读：struct tag 分别控制 JSON 跨进程存储与 YAML 输入字段名；
// omitempty 只控制编码时省略零值，不会自动校验字段是否必填。
// Target 是包选择器，Version 是被插桩依赖的版本范围，Where 保留额外筛选条件。
type InstBaseRule struct {
	Name    string            `json:"name,omitempty"    yaml:"name,omitempty"`
	Target  Target            `json:"target"            yaml:"target"`
	Version string            `json:"version,omitempty" yaml:"version,omitempty"`
	Imports map[string]string `json:"imports,omitempty" yaml:"imports,omitempty"`
	Where   *WhereDef         `json:"where,omitempty"   yaml:"where,omitempty"`
}

func (ibr *InstBaseRule) String() string      { return ibr.Name }
func (ibr *InstBaseRule) GetName() string     { return ibr.Name }
func (ibr *InstBaseRule) GetTarget() *Target  { return &ibr.Target }
func (ibr *InstBaseRule) GetVersion() string  { return ibr.Version }
func (ibr *InstBaseRule) GetWhere() *WhereDef { return ibr.Where }

// InstRuleSet represents a collection of instrumentation rules that apply to a
// single Go package within a specific module. It acts as a container for rules,
// organizing them by file and by the specific functions or structs they target.
// This structure is essential for the instrumentation process, as it allows the
// tool to efficiently locate and apply the correct rules to the source code.
// 中文导读：这是匹配后发送给编译进程的工作单。
// ModulePath 字段实际保存 NewInstRuleSet 收到的包 import path，不要被名字误导为 go.mod
// 中的 module path；同一个 module 通常包含多个包，也就可能形成多个规则集合。
// 各 map 用绝对源文件路径作键、规则指针切片作值；FileRules 则是按包增加文件的规则。
type InstRuleSet struct {
	PackageName    string                          `json:"package_name"`
	ModulePath     string                          `json:"module_path"`
	CgoFileMap     map[string]string               `json:"cgo_file_map,omitempty"` // go -> cgo
	RawRules       map[string][]*InstRawRule       `json:"raw_rules"`
	FuncRules      map[string][]*InstFuncRule      `json:"func_rules"`
	StructRules    map[string][]*InstStructRule    `json:"struct_rules"`
	CallRules      map[string][]*InstCallRule      `json:"call_rules"`
	LitRules       map[string][]*InstLitRule       `json:"lit_rules"`
	DirectiveRules map[string][]*InstDirectiveRule `json:"directive_rules"`
	DeclRules      map[string][]*InstDeclRule      `json:"decl_rules"`
	FileRules      []*InstFileRule                 `json:"file_rules"`
	// IgnoredCallFiles lists source files that hold a //otelc:ignore comment
	// above a call.
	IgnoredCallFiles []string `json:"ignored_call_files,omitempty"`
}

func NewInstRuleSet(importPath string) *InstRuleSet {
	// map 的零值是 nil：读取安全，写入会 panic，因此构造时先 make 初始化各个映射。
	// slice 的零值则可直接 append；这里显式创建空 FileRules 也让 JSON 空集合表达稳定。
	return &InstRuleSet{
		PackageName:    "",
		ModulePath:     importPath,
		CgoFileMap:     make(map[string]string),
		RawRules:       make(map[string][]*InstRawRule),
		FuncRules:      make(map[string][]*InstFuncRule),
		StructRules:    make(map[string][]*InstStructRule),
		CallRules:      make(map[string][]*InstCallRule),
		LitRules:       make(map[string][]*InstLitRule),
		DirectiveRules: make(map[string][]*InstDirectiveRule),
		DeclRules:      make(map[string][]*InstDeclRule),
		FileRules:      make([]*InstFileRule, 0),
	}
}

func (irs *InstRuleSet) String() string {
	parts := []string{
		fmt.Sprintf("raw=%v", irs.RawRules),
		fmt.Sprintf("func=%v", irs.FuncRules),
		fmt.Sprintf("struct=%v", irs.StructRules),
		fmt.Sprintf("call=%v", irs.CallRules),
		fmt.Sprintf("lit=%v", irs.LitRules),
		fmt.Sprintf("directive=%v", irs.DirectiveRules),
		fmt.Sprintf("decl=%v", irs.DeclRules),
		fmt.Sprintf("file=%v", irs.FileRules),
	}
	return fmt.Sprintf("{%s: %s}", irs.ModulePath, strings.Join(parts, ", "))
}

func (irs *InstRuleSet) IsEmpty() bool {
	// 调用点 ignore 标记也会影响编译期行为，所以只有标记而无普通规则时仍然不是空集。
	return irs == nil ||
		(len(irs.FuncRules) == 0 &&
			len(irs.StructRules) == 0 &&
			len(irs.RawRules) == 0 &&
			len(irs.CallRules) == 0 &&
			len(irs.LitRules) == 0 &&
			len(irs.DirectiveRules) == 0 &&
			len(irs.DeclRules) == 0 &&
			len(irs.FileRules) == 0 &&
			len(irs.IgnoredCallFiles) == 0)
}

// AddRule is a generic method that adds any type of rule to the appropriate map.
// It works with any rule type that implements the InstRule interface.
func addRule[T InstRule](file string, rule T, rulesMap map[string][]T) {
	// 泛型参数 T 受 InstRule 约束，复用追加逻辑而保留具体规则类型的静态检查。
	// append 可能返回新的底层数组，必须把结果写回 map，而不是仅调用 append。
	util.Assert(filepath.IsAbs(file), "file must be an absolute path")
	rulesMap[file] = append(rulesMap[file], rule)
}

func (irs *InstRuleSet) AddRawRule(file string, rule *InstRawRule) {
	addRule(file, rule, irs.RawRules)
}

func (irs *InstRuleSet) AddFuncRule(file string, rule *InstFuncRule) {
	addRule(file, rule, irs.FuncRules)
}

func (irs *InstRuleSet) AddStructRule(file string, rule *InstStructRule) {
	addRule(file, rule, irs.StructRules)
}

func (irs *InstRuleSet) AddCallRule(file string, rule *InstCallRule) {
	addRule(file, rule, irs.CallRules)
}

func (irs *InstRuleSet) AddLitRule(file string, rule *InstLitRule) {
	addRule(file, rule, irs.LitRules)
}

func (irs *InstRuleSet) AddDirectiveRule(file string, rule *InstDirectiveRule) {
	addRule(file, rule, irs.DirectiveRules)
}

func (irs *InstRuleSet) AddDeclRule(file string, rule *InstDeclRule) {
	addRule(file, rule, irs.DeclRules)
}

// AddIgnoredCallFile records that file holds a //otelc:ignore comment above
// a call.
func (irs *InstRuleSet) AddIgnoredCallFile(file string) {
	util.Assert(filepath.IsAbs(file), "file must be an absolute path")
	irs.IgnoredCallFiles = append(irs.IgnoredCallFiles, file)
}

func (irs *InstRuleSet) AddFileRule(rule *InstFileRule) {
	irs.FileRules = append(irs.FileRules, rule)
}

func (irs *InstRuleSet) SetPackageName(name string) {
	util.Assert(name != "", "package name is empty")
	irs.PackageName = name
}

// SetCgoFileMap sets the CGO file mapping for this rule set.
func (irs *InstRuleSet) SetCgoFileMap(cgoFiles map[string]string) {
	irs.CgoFileMap = cgoFiles
}

// AllFuncRules returns all function rules from the rule set as a flat slice.
func (irs *InstRuleSet) AllFuncRules() []*InstFuncRule {
	n := 0
	for _, rs := range irs.FuncRules {
		n += len(rs)
	}
	rules := make([]*InstFuncRule, 0, n)
	for _, rs := range irs.FuncRules {
		rules = append(rules, rs...)
	}
	return rules
}

// AllStructRules returns all struct rules from the rule set as a flat slice.
func (irs *InstRuleSet) AllStructRules() []*InstStructRule {
	n := 0
	for _, rs := range irs.StructRules {
		n += len(rs)
	}
	rules := make([]*InstStructRule, 0, n)
	for _, rs := range irs.StructRules {
		rules = append(rules, rs...)
	}
	return rules
}
