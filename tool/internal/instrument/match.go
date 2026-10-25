// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package instrument

import (
	"encoding/json"
	"os"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/internal/rule"
	"go.opentelemetry.io/otelc/tool/util"
)

// load loads the matched rules from the build temp directory.
// TODO: Shared memory across all sub-processes is possible
// 中文导读：setup 与 compile 拦截是不同阶段/进程，靠构建临时目录中的 JSON
// 传递已解析的规则；这里并不重新扫描所有 instrumentation 目录做规则发现。
func (ip *instrumentPhase) load() ([]*rule.InstRuleSet, error) {
	f := util.GetMatchedRuleFile()
	content, err := os.ReadFile(f)
	if os.IsNotExist(err) {
		return nil, ex.Newf("no instrumentation configuration found (%s does not exist); "+
			"run `otelc setup` in the module directory before building with `-toolexec`, "+
			"or build with `otelc go build` instead", f)
	}
	if err != nil {
		return nil, ex.Wrapf(err, "failed to read file %s", f)
	}
	rset := make([]*rule.InstRuleSet, 0)
	err = json.Unmarshal(content, &rset)
	if err != nil {
		return nil, ex.Wrapf(err, "failed to unmarshal JSON")
	}

	ip.Debug("Load matched rule sets", "path", f)
	return rset, nil
}

// anyRuleSetHasIgnoredCallFiles reports whether any package in this build has
// a //otelc:ignore comment above a call, found during setup.
func anyRuleSetHasIgnoredCallFiles(allSet []*rule.InstRuleSet) bool {
	for _, rset := range allSet {
		if len(rset.IgnoredCallFiles) > 0 {
			return true
		}
	}
	return false
}

// loadImportNames loads the import name table. A missing file is not
// an error, since the table only improves a fallback guess.
func loadImportNames() (map[string]string, error) {
	f := util.GetImportNamesFile()
	content, err := os.ReadFile(f)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, ex.Wrapf(err, "failed to read file %s", f)
	}
	names := make(map[string]string)
	if jsonErr := json.Unmarshal(content, &names); jsonErr != nil {
		return nil, ex.Wrapf(jsonErr, "failed to unmarshal JSON")
	}
	return names, nil
}

// match matches the rules with the compile command.
// 中文导读：此处匹配的是 compile -p 指定的包导入路径。字段虽叫 ModulePath，
// 此处用途是与编译包路径比较，不要将它理解成只匹配 go.mod 的 module 根路径。
// setup 已筛过函数名、接收者、版本等条件；当前先取对应包的规则集，应用时
// 还需在重新解析的 AST 中定位目标函数，不能直接跨进程复用旧语法树节点。
func (ip *instrumentPhase) match(allSet []*rule.InstRuleSet, args []string) *rule.InstRuleSet {
	// One package can only be matched with one rule set, so it's safe to return
	// the first matched rule set.
	importPath := util.FindFlagValue(args, "-p")
	util.Assert(importPath != "", "sanity check")
	for _, rset := range allSet {
		if rset.ModulePath == importPath {
			ip.Debug("Match rule set", "set", rset)
			return rset
		}
	}
	return nil
}
