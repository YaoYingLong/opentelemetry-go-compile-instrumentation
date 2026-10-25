// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 规则文件有两个独立维度：顶层 version 限制工具版本，每条规则的 version
// 限制目标依赖版本。本文件负责解析前者，并把条目交给 Normalize 生成可执行规则。
package rule

import (
	"sort"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/util"
)

const LegacyVersion = "v1.0.0"

// Entry is a named rule file entry that can be decoded by its consumer.
type Entry struct {
	Name string
	Node yaml.Node
}

// File is a parsed rule file with entries sorted by name.
type File struct {
	MinimumVersion string
	Legacy         bool
	Entries        []Entry
}

func ParseFile(data []byte) (File, error) {
	// 先解成 yaml.Node，保留每个条目的结构，再按名字排序；不直接解为 map，
	// 是为了显式校验重复键、顶层类型与 version，并让条目处理顺序可预测。
	// 没写顶层 version 的文件走 legacy 路径，匹配加载阶段会对此给出提示。
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return File{}, err
	}
	if len(root.Content) == 0 {
		return File{MinimumVersion: LegacyVersion, Legacy: true}, nil
	}

	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return File{}, ex.New("rule file root must be a mapping")
	}

	result := File{MinimumVersion: LegacyVersion, Legacy: true}
	seen := make(map[string]struct{})
	for i := 0; i < len(doc.Content); i += 2 {
		key := doc.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return File{}, ex.New("rule file mapping keys must be strings")
		}
		name := key.Value
		if _, ok := seen[name]; ok {
			return File{}, ex.Newf("mapping key %q already defined", name)
		}
		seen[name] = struct{}{}

		value := doc.Content[i+1]
		if name != "version" {
			result.Entries = append(result.Entries, Entry{Name: name, Node: *value})
			continue
		}

		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return File{}, ex.New("minimum otelc version must be a string")
		}
		if !semver.IsValid(value.Value) || module.IsPseudoVersion(value.Value) {
			return File{}, ex.Newf("minimum otelc version %q is not a valid release version", value.Value)
		}
		result.MinimumVersion = value.Value
		result.Legacy = false
	}

	sort.Slice(result.Entries, func(i, j int) bool {
		return result.Entries[i].Name < result.Entries[j].Name
	})
	return result, nil
}

// Rules constructs the executable instrumentation rules in this file.
func (f File) Rules() ([]InstRule, error) {
	// Normalize 兼容规则表面语法并展开组合条目：一个 YAML 名字可能产生多条规则。
	// 每条归一化结果再按选择器创建具体结构，统一检查 target 与依赖版本范围。
	// 新组件开发应先对照已有组件当前配置，不能直接把旧设计文档中的示例当格式规范。
	rules := make([]InstRule, 0)
	for _, entry := range f.Entries {
		var fields map[string]any
		if err := entry.Node.Decode(&fields); err != nil {
			return nil, ex.Wrapf(err, "parsing rule %q", entry.Name)
		}
		flatRules, err := Normalize(fields)
		if err != nil {
			return nil, err
		}
		for _, flatFields := range flatRules {
			raw, marshalErr := yaml.Marshal(flatFields)
			if marshalErr != nil {
				return nil, ex.Wrap(marshalErr)
			}
			r, ruleErr := newRule(raw, entry.Name, flatFields)
			if ruleErr != nil {
				return nil, ruleErr
			}
			if r.GetTarget().IsZero() {
				return nil, ex.Newf("rule %q has an empty target; target is required", entry.Name)
			}
			if validateErr := r.GetTarget().Validate(); validateErr != nil {
				return nil, ex.Wrapf(validateErr, "rule %q", entry.Name)
			}
			if validateErr := util.ValidateVersionRange(r.GetVersion()); validateErr != nil {
				return nil, ex.Wrapf(validateErr, "rule %q", entry.Name)
			}
			rules = append(rules, r)
		}
	}
	return rules, nil
}

func newRule(raw []byte, name string, fields map[string]any) (InstRule, error) {
	// 规则类型取决于归一化后的 selector，不取决于 YAML 条目名或 hook 函数名。
	// switch 分支顺序是实现的一部分；新增语法需确认 Normalize 与这里的分派保持一致。
	switch {
	case fields[selStruct] != nil:
		return NewInstStructRule(raw, name)
	case fields[whereFile] != nil:
		return NewInstFileRule(raw, name)
	case fields[selDirective] != nil:
		return NewInstDirectiveRule(raw, name)
	case fields[rawField] != nil:
		return NewInstRawRule(raw, name)
	case fields[selFunc] != nil:
		return NewInstFuncRule(raw, name)
	case fields[selFunctionCall] != nil, fields[selMethodCall] != nil:
		return NewInstCallRule(raw, name)
	case fields[selStructLiteral] != nil:
		return NewInstLitRule(raw, name)
	case fields[selIdentifier] != nil:
		return NewInstDeclRule(raw, name)
	default:
		return nil, ex.Newf("rule %q has no recognised selector", name)
	}
}

func CheckVersion(current, required string) error {
	// 开发构建/伪版本无法可靠按正式发布版本判断兼容性，此处放行。
	// 这只影响工具最低版本检查，不意味着函数/包选择或依赖版本筛选也会被跳过。
	if !semver.IsValid(current) || current == "v0.0.0" || module.IsPseudoVersion(current) {
		return nil
	}
	if semver.Compare(current, required) < 0 {
		return ex.Newf("requires otelc >= %s, current version is %s", required, current)
	}
	return nil
}
