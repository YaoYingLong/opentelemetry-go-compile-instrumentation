// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"go.opentelemetry.io/otel/trace"
)

// 中文导读：这是组件代码读取 GLS 的桥梁。普通 Go 标准库没有这里需要的
// runtime.GetTraceContextFromGLS；该能力由插桩器注入。因此本包先提供安全的
// 默认函数，真正的实现由注入 SDK 的 otel_trace_context.go 在 init 中注册。
// 函数在 Go 中也是值，可以赋给变量、作为参数传入并在稍后调用。
var (
	traceAndSpanIDFunc = defaultTraceAndSpanID
	spanFromGLSFunc    = defaultSpanFromGLS
)

func defaultTraceAndSpanID() (string, string) {
	return "", ""
}

func defaultSpanFromGLS() trace.Span {
	return nil
}

// GetTraceAndSpanID 返回当前 goroutine 上可见的追踪 ID，供日志关联等场景使用。
// 两个 string 是 Go 的多返回值；未注册实现或没有当前 Span 时返回两个空字符串。
func GetTraceAndSpanID() (string, string) {
	return traceAndSpanIDFunc()
}

// RegisterTraceAndSpanIDFunc 在初始化阶段替换默认实现。
// 这里是普通函数变量赋值，不是支持并发热替换的注册中心。
func RegisterTraceAndSpanIDFunc(f func() (string, string)) {
	traceAndSpanIDFunc = f
}

// GetSpanFromGLS 返回当前 goroutine 的隐式 Span；调用方必须处理 nil。
// 显式请求 context 的查找优先级由 trace/SpanFromContext 的 After hook 决定。
func GetSpanFromGLS() trace.Span {
	return spanFromGLSFunc()
}

func RegisterSpanFromGLSFunc(f func() trace.Span) {
	spanFromGLSFunc = f
}
