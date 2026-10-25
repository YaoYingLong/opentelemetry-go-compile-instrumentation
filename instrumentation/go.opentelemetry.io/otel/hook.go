// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"sync/atomic"

	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/hook"
)

var tracerProviderSet atomic.Bool

// 拦截otel.SetTracerProvider，只放行第一次调用，后续调用直接跳过，防止全局TracerProvider被再次替换
func beforeSetTracerProvider(ictx hook.HookContext, _ trace.TracerProvider) {
	if tracerProviderSet.Swap(true) {
		ictx.SetSkipCall(true)
	}
}
