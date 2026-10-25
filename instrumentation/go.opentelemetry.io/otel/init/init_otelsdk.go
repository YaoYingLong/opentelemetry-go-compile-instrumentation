//go:build ignore

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go.opentelemetry.io/otelc/pkg/runtime"
)

// 向应用的 main 包注入 init()，自动初始化 OpenTelemetry SDK，并启动 Go runtime 指标采集。
func init() {
	// add_file 把本文件编入应用 main 包，因此这段在 main.main 之前执行。
	// 不需要用户手动调用，但也不要误认为它早于所有依赖包的 init：
	// Go 会先初始化依赖包，再初始化导入它们的 main 包。
	// Initialize OpenTelemetry SDK (sets up global tracer and meter providers)
	runtime.SetupOTelSDK()

	// Start runtime metrics (respects OTEL_GO_ENABLED/DISABLED_INSTRUMENTATIONS)
	runtime.StartRuntimeMetrics()
}
