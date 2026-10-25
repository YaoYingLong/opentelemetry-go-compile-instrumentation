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
	// Swap 原子地“读取旧值并写入 true”。只有首次读取到 false 的调用通过；
	// 后续调用返回旧值 true，要求 trampoline 跳过被拦截的 SetTracerProvider。
	// 这是本项目当前实现的策略，不能推广成 OTel API 天生不允许更换 Provider。
	if tracerProviderSet.Swap(true) {
		ictx.SetSkipCall(true)
	}
}
