// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace

import (
	_ "unsafe"

	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/hook"
)

// 这里属于插桩 hook 包；真正维护栈的代码在 otel_trace_context.go 中，
// 该文件会被 add_file 注入 OTel SDK 的 trace 包。linkname 连接两个包的符号，
// 声明只有签名、没有函数体并不是遗漏实现；_ "unsafe" 满足该指令的要求。
// //go:linkname 是编译指令，不能当普通说明随意删除或挪动。

//go:linkname traceContextAddSpan go.opentelemetry.io/otel/sdk/trace.traceContextAddSpan
func traceContextAddSpan(span trace.Span)

//go:linkname traceContextDelSpan go.opentelemetry.io/otel/sdk/trace.traceContextDelSpan
func traceContextDelSpan(span trace.Span)

func afterNewRecordingSpan(_ hook.HookContext, span any) {
	addSpanToGLS(span)
}

func afterNewNonRecordingSpan(_ hook.HookContext, span any) {
	// 没有记录事件/属性的 Span 也可能具有有效的 TraceID/SpanID，
	// 仍需要参与父子关系传播，不能只把 recording Span 加入 GLS。
	addSpanToGLS(span)
}

func beforeRecordingSpanEnd(_ hook.HookContext, span any, _ ...any) {
	// End 的接收者是第一个原始参数；_ 表示有意忽略参数。
	// ...any 是可变参数，用于接收规则允许匹配的其余参数。
	deleteSpanFromGLS(span)
}

func beforeNonRecordingSpanEnd(_ hook.HookContext, span any, _ ...any) {
	deleteSpanFromGLS(span)
}

// 在 Span 创建后，将其加入当前 goroutine 的本地存储（GLS）；在 Span 结束时移除。包括 recording 和 non-recording Span
func addSpanToGLS(span any) {
	if span != nil {
		if s, ok := span.(trace.Span); ok {
			// 类型断言把 any 中的动态值视为 trace.Span 接口；使用 ok 分支避免
			// 类型不匹配时 panic。并不意味着 s 一定是某个可导出的 SDK 结构体。
			traceContextAddSpan(s)
		}
	}
}

func deleteSpanFromGLS(span any) {
	if span != nil {
		if s, ok := span.(trace.Span); ok {
			traceContextDelSpan(s)
		}
	}
}
