// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace

import (
	_ "unsafe"

	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/hook"
)

//go:linkname traceContextAddSpan go.opentelemetry.io/otel/sdk/trace.traceContextAddSpan
func traceContextAddSpan(span trace.Span)

//go:linkname traceContextDelSpan go.opentelemetry.io/otel/sdk/trace.traceContextDelSpan
func traceContextDelSpan(span trace.Span)

func afterNewRecordingSpan(_ hook.HookContext, span any) {
	addSpanToGLS(span)
}

func afterNewNonRecordingSpan(_ hook.HookContext, span any) {
	addSpanToGLS(span)
}

func beforeRecordingSpanEnd(_ hook.HookContext, span any, _ ...any) {
	deleteSpanFromGLS(span)
}

func beforeNonRecordingSpanEnd(_ hook.HookContext, span any, _ ...any) {
	deleteSpanFromGLS(span)
}

// 在 Span 创建后，将其加入当前 goroutine 的本地存储（GLS）；在 Span 结束时移除。包括 recording 和 non-recording Span
func addSpanToGLS(span any) {
	if span != nil {
		if s, ok := span.(trace.Span); ok {
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
