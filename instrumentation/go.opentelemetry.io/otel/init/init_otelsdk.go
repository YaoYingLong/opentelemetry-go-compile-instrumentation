//go:build ignore

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go.opentelemetry.io/otelc/pkg/runtime"
)

// 向应用的 main 包注入 init()，自动初始化 OpenTelemetry SDK，并启动 Go runtime 指标采集。
func init() {
	// Initialize OpenTelemetry SDK (sets up global tracer and meter providers)
	runtime.SetupOTelSDK()

	// Start runtime metrics (respects OTEL_GO_ENABLED/DISABLED_INSTRUMENTATIONS)
	runtime.StartRuntimeMetrics()
}
