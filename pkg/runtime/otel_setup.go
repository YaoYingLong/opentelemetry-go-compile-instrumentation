// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"os"
	"strings"
	"sync/atomic"
)

// configCache 保存一次环境变量解析结果。发布后只读；环境变量改变时创建
// 新对象替换旧对象，避免多个 goroutine 同时修改同一个 map。
type configCache struct {
	sdkDisabled   string
	enabledRaw    string
	disabledRaw   string
	isSdkDisabled bool
	hasEnabled    bool
	enabledMap    map[string]struct{}
	disabledMap   map[string]struct{}
}

// atomic.Pointer[T] 是泛型原子指针：Load/Store 使整个缓存对象的发布和读取安全。
// 它不会自动让指针所指对象中的 map 变成并发可写，所以必须遵守上面的只读约定。
var instConfigCache atomic.Pointer[configCache]

// SetupOTelSDK initializes the OpenTelemetry SDK.
//
// The SDK automatically configures exporters based on environment variables
// following the OpenTelemetry specification:
//
// SDK Configuration:
//   - OTEL_SDK_DISABLED: If set to the case-insensitive string "true", the SDK
//     is disabled entirely and no providers are installed. Every other value
//     (including unset) leaves the SDK enabled, per the OpenTelemetry
//     specification.
//
// Service Configuration (highest to lowest precedence):
//   - OTEL_RESOURCE_ATTRIBUTES: Key-value pairs (e.g., "service.name=myapp,service.version=1.2.3")
//   - OTEL_SERVICE_NAME: Service name for telemetry
//
// Exporter Configuration (applies independently to each signal; traces,
// metrics, and logs are all enabled by default and configured symmetrically):
//   - OTEL_TRACES_EXPORTER: Traces exporter: otlp (default), console, none
//   - OTEL_METRICS_EXPORTER: Metrics exporter: otlp (default), console, prometheus, none
//   - OTEL_LOGS_EXPORTER: Logs exporter: otlp (default), console, none
//   - OTEL_EXPORTER_OTLP_ENDPOINT: OTLP endpoint for all signals (default: http://localhost:4318)
//   - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: Traces-specific endpoint override
//   - OTEL_EXPORTER_OTLP_METRICS_ENDPOINT: Metrics-specific endpoint override
//   - OTEL_EXPORTER_OTLP_LOGS_ENDPOINT: Logs-specific endpoint override
//   - OTEL_EXPORTER_OTLP_PROTOCOL: Protocol (grpc, http/protobuf, http/json)
//   - OTEL_EXPORTER_PROMETHEUS_HOST: Prometheus exporter host (default: localhost)
//   - OTEL_EXPORTER_PROMETHEUS_PORT: Prometheus exporter port (default: 9464)
//
// When the exporter for a signal defaults to "otlp" and no endpoint override
// is set, telemetry for that signal is sent to the OTLP-spec default endpoint
// (http://localhost:4318). This means traces, metrics, and logs are exported
// by default even when no OTLP endpoint is configured; set the relevant
// *_EXPORTER variable to "none" to disable a signal explicitly.
//
// Other Configuration:
//   - OTEL_PROPAGATORS: Comma-separated propagators (tracecontext, baggage, b3,
//     b3multi, jaeger, xray, ottrace, none). Default: "tracecontext,baggage"
//   - OTEL_LOG_LEVEL: Log level (debug, info, warn, error)
func SetupOTelSDK() {
	// 如果配置了disabled直接退出
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		Logger().Info("OpenTelemetry SDK disabled via OTEL_SDK_DISABLED=true, skipping initialization")
		return
	}

	// Initialize OpenTelemetry SDK with defensive error handling
	Initialize(Config{
		InstrumentationName:    "go.opentelemetry.io/otelc",
		InstrumentationVersion: ModuleVersion(),
	})
}

// Instrumented checks if instrumentation is enabled via environment variables.
//
// Environment variables (following OTel JS pattern):
//   - OTEL_SDK_DISABLED: If set to the case-insensitive string "true", all instrumentations are disabled
//   - OTEL_GO_ENABLED_INSTRUMENTATIONS: comma-separated list of enabled instrumentations (e.g., "nethttp,grpc")
//   - OTEL_GO_DISABLED_INSTRUMENTATIONS: comma-separated list of disabled instrumentations (e.g., "nethttp")
//
// Logic:
//  1. If OTEL_SDK_DISABLED is "true", returns false
//  2. If OTEL_GO_ENABLED_INSTRUMENTATIONS is set, only those instrumentations are enabled
//  3. Then OTEL_GO_DISABLED_INSTRUMENTATIONS is applied to disable specific ones
//  4. If neither is set, all instrumentations are enabled by default
//
// The instrumentationName should be lowercase (e.g., "nethttp", "grpc").
//
// The three env vars above are still read via os.Getenv on every call; what's
// cached is the parsed list -> map[string]struct{} conversion, so repeated
// calls skip re-splitting and re-allocating those lists as long as the env
// vars haven't changed. See BenchmarkInstrumented for the unset vs.
// configured-list allocation counts this caching actually buys.
func Instrumented(instrumentationName string) bool {
	// 运行期开关供组件决定是否创建/记录遥测；框架仍可能调用 hook，After 也可能
	// 需要收尾。它不能让二进制获得构建时未选中的组件，与 setup 选规则是不同层次。
	sdkDisabled := os.Getenv("OTEL_SDK_DISABLED")
	enabledList := os.Getenv("OTEL_GO_ENABLED_INSTRUMENTATIONS")
	disabledList := os.Getenv("OTEL_GO_DISABLED_INSTRUMENTATIONS")

	cached := instConfigCache.Load()
	// 每次仍读环境变量，缓存的是字符串拆分后的集合；并发读到相同变化时，
	// 多个 goroutine 各自构建相同快照也可接受，因为不会修改已发布对象。
	if cached == nil || cached.sdkDisabled != sdkDisabled || cached.enabledRaw != enabledList ||
		cached.disabledRaw != disabledList {
		cached = buildConfigCache(sdkDisabled, enabledList, disabledList)
		instConfigCache.Store(cached)
	}

	if cached.isSdkDisabled {
		return false
	}

	name := strings.ToLower(instrumentationName)

	if cached.hasEnabled {
		// map 查询的第二个结果 ok 表示键是否存在；这里把 map 当作集合。
		// 启用列表非空时先做白名单，再检查禁用列表，因此禁用优先。
		if _, ok := cached.enabledMap[name]; !ok {
			return false
		}
	}

	if _, ok := cached.disabledMap[name]; ok {
		return false
	}

	return true
}

func buildConfigCache(sdkDisabled, enabledList, disabledList string) *configCache {
	c := &configCache{
		sdkDisabled:   sdkDisabled,
		enabledRaw:    enabledList,
		disabledRaw:   disabledList,
		isSdkDisabled: strings.EqualFold(sdkDisabled, "true"),
	}

	if parsed := parseInstrumentationList(enabledList); len(parsed) > 0 {
		// map[string]struct{} 表示字符串集合；空结构体作为值不携带额外业务数据。
		// 只有空格/逗号的列表解析后为空，不会形成“禁止全部组件”的白名单。
		c.hasEnabled = true
		c.enabledMap = make(map[string]struct{})
		for _, item := range parsed {
			c.enabledMap[item] = struct{}{}
		}
	}

	if disabledList != "" {
		c.disabledMap = make(map[string]struct{})
		for _, item := range parseInstrumentationList(disabledList) {
			c.disabledMap[item] = struct{}{}
		}
	}

	return c
}

// parseInstrumentationList parses a comma-separated list of instrumentation names.
func parseInstrumentationList(list string) []string {
	var result []string
	// SplitSeq 返回可供 range 遍历的迭代器；每轮只处理一个片段。
	// nil 切片可直接 append，省去了显式初始化空切片。
	for item := range strings.SplitSeq(list, ",") {
		trimmed := strings.TrimSpace(strings.ToLower(item))
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
