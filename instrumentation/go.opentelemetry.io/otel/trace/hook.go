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
	if !span.SpanContext().IsValid() {
		glsSpan := runtime.GetSpanFromGLS()
		if glsSpan != nil {
			ictx.SetReturnVal(0, glsSpan)
		}
	}
}
