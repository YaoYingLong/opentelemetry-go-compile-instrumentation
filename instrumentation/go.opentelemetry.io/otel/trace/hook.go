// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace

import (
	_ "unsafe"

	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/hook"
	"go.opentelemetry.io/otelc/pkg/runtime"
)

// 拦截 trace.SpanFromContext(ctx)：如果从 ctx 取出的 Span 无效，就尝试返回 GLS 中的当前 Span。
func afterSpanFromContext(ictx hook.HookContext, span trace.Span) {
	// 原函数已经先查过传入的 context。有效结果直接保留，GLS 只是无有效
	// Span 时的补充；因此显式 context 仍可决定请求的父子关系。
	if !span.SpanContext().IsValid() {
		glsSpan := runtime.GetSpanFromGLS()
		if glsSpan != nil {
			// After 收到的是返回值的副本；修改局部 span 不会替换调用者拿到的值。
			// 通过 HookContext 的返回值槽位 0 写回，才会改变这次函数调用的结果。
			ictx.SetReturnVal(0, glsSpan)
		}
	}
}
