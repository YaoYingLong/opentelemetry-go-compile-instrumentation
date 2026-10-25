// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 函数规则连接“匹配哪个函数”和“执行哪个 hook”，是新增常规组件时的核心配置模型。
// 可按 Target/Version/Func/Recv 理解匹配端，再按 Path/Before/After 理解 hook 端；
// 这些值先被 setup 匹配和持久化，再由 instrument 生成 trampoline 与运行时绑定。
package rule

import (
	"strconv"
	"strings"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/util"
	"go.yaml.in/yaml/v3"
)

// FuncSignature specifies the argument and result types used by signature
// sub-filters on InstFuncRule.  Each entry is a type name string in the form
// accepted by the type-name parser (e.g. "error", "context.Context",
// "*http.Request").  For exact matching (signature), the i-th entry is
// compared to the i-th field in order.  For contains matching
// (signature_contains), each entry is checked for presence anywhere in the
// corresponding list.
// 中文导读：这里描述的是目标 Go 函数的类型列表，而不是 hook 函数的类型列表。
// 精确签名按顺序和数量比较；contains 是“任一命中”：任一指定参数类型出现在参数中，
// 或任一指定返回类型出现在返回值中即通过，并非要求列出的所有类型都存在。
// *FuncSignature 指针使“未指定子过滤器”可用 nil 表达，与显式提供空签名区分开。
type FuncSignature struct {
	Args    []string `json:"args,omitempty"    yaml:"args"`
	Returns []string `json:"returns,omitempty" yaml:"returns"`
}

// InstFuncRule represents a rule that guides hook function injection into
// appropriate target function locations. For example, if we want to inject
// custom Foo function at the entry of target function Bar, we can define a rule:
//
//	rule:
//		name: "newrule"
//		target: "main"
//		func: "Bar"
//		recv: "*RecvType"
//		before: "Foo"
//		path: "github.com/foo/bar/hook_rule"
//
// Optional signature sub-filters narrow matching beyond name and receiver:
//
//	signature:
//	  args: [context.Context, string]
//	  returns: [error]
//	signature_contains:
//	  args: [context.Context]
//	result: error
//	last_result: error
//	param: context.Context
// 中文导读：InstBaseRule 的匿名嵌入让 Name/Target/Version 等公共字段和方法可直接使用；
// yaml:",inline" 令它们在 YAML 中与 func、before 等字段处于同一层，而非额外嵌套。
// Recv 区分普通函数与方法，并区分 T、*T；Before/After 是 hook 包内的函数名。
// Path 是 hook 的 import path，ResolvedPath 是 setup 解析出的本地目录，不接受 YAML 输入。
// Result/LastResult/Param 继续过滤目标函数的返回值或参数类型，不是采集属性的配置。
type InstFuncRule struct {
	InstBaseRule `yaml:",inline"`

	Func              string         `json:"func"                         yaml:"func"`
	Recv              string         `json:"recv"                         yaml:"recv"`
	Before            string         `json:"before"                       yaml:"before"`
	After             string         `json:"after"                        yaml:"after"`
	Path              string         `json:"path"                         yaml:"path"`
	ResolvedPath      string         `json:"resolved_path"                yaml:"-"` // local dir Path resolves to
	Signature         *FuncSignature `json:"signature,omitempty"          yaml:"signature"`
	SignatureContains *FuncSignature `json:"signature_contains,omitempty" yaml:"signature_contains"`
	Result            string         `json:"result,omitempty"             yaml:"result"`
	LastResult        string         `json:"last_result,omitempty"        yaml:"last_result"`
	Param             string         `json:"param,omitempty"              yaml:"param"`
}

// NewInstFuncRule loads and validates an InstFuncRule from YAML data.
func NewInstFuncRule(data []byte, name string) (*InstFuncRule, error) {
	// YAML 解码后只做函数规则自身的必要字段检查；target 和版本范围的通用校验
	// 在 File.Rules 中完成。看到 validate 较短，不代表整个规则格式没有其他约束。
	var r InstFuncRule
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, ex.Wrap(err)
	}
	if r.Name == "" {
		r.Name = name
	}
	if err := r.validate(); err != nil {
		return nil, ex.Wrapf(err, "invalid func rule %q", name)
	}
	return &r, nil
}

func (r *InstFuncRule) validate() error {
	if strings.TrimSpace(r.Func) == "" {
		return ex.Newf("func cannot be empty")
	}
	if strings.TrimSpace(r.Before) == "" && strings.TrimSpace(r.After) == "" {
		return ex.Newf("before or after must be set")
	}
	if strings.TrimSpace(r.Path) == "" {
		return ex.Newf("path cannot be empty")
	}
	return nil
}

// Identity returns a content-derived key used to generate trampoline and
// HookContext names. It is a function purely of what the rule does — its
// target, function/receiver, before/after hooks, hook path, and signature
// filters — never of the rule's name or its position in a do sequence.
//
// De-duplication: two rules that do the same thing share an identity, so they
// collapse to a single generated artifact instead of redeclaring it. The only
// remaining collision is between byte-identical rules, which are effectively the
// same rule. A rule's position in a do sequence is intentionally excluded — it
// does not change what a rule does; do-sequence order is preserved by the order
// in which the expanded rules are applied, not by the generated name.
//
// Deriving the identity from content (rather than a "name#index" string) closes
// the collision in issue #560, where a rule literally named "name#index" at
// application index 0 rendered the same string as "name" at index N.
//
// When adding a field to InstFuncRule that changes the generated instrumentation,
// include it here so the identity stays faithful to what the rule does.
//
// The key is built with explicit length prefixes ("len:value") instead of a
// delimiter, so it is injective for arbitrary field content. Type-name strings
// may legitimately contain any character — commas in function types, "|" in
// type constraints — so no printable separator is truly reserved; the length
// prefix marks where each value ends without relying on its content.
func (r *InstFuncRule) Identity() string {
	// 按行为生成稳定名称，而非用可重复的配置名称。新增影响生成代码的字段时，
	// 需要同步这里，否则不同行为可能被误认为同一份生成物。
	// 长度前缀先保证拼接编码无歧义，再用 CRC32 生成紧凑标识；CRC32 本身不是无碰撞哈希。
	enc := func(s string) string { return strconv.Itoa(len(s)) + ":" + s }
	encList := func(xs []string) string {
		encs := make([]string, len(xs))
		for i, x := range xs {
			encs[i] = enc(x)
		}
		return strconv.Itoa(len(xs)) + ";" + strings.Join(encs, "")
	}
	encSig := func(s *FuncSignature) string {
		if s == nil {
			return "-" // absent: distinct from a present-but-empty signature
		}
		return "+" + encList(s.Args) + encList(s.Returns)
	}
	parts := []string{
		enc(r.Target.String()), enc(r.Version), enc(r.Func), enc(r.Recv),
		enc(r.Before), enc(r.After), enc(r.Path),
		enc(r.Result), enc(r.LastResult), enc(r.Param),
		encSig(r.Signature), encSig(r.SignatureContains),
	}
	return util.CRC32(strings.Join(parts, ""))
}
