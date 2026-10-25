// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 中文导读：本包是注入到 net/http 的运行时逻辑，匹配位置见同目录 otelc.yaml。
// 可按 BeforeRoundTrip → 原始 (*http.Transport).RoundTrip → AfterRoundTrip 阅读。
// 类似 Java 的方法入口/出口 Advice，但这里在编译时改写 Go 源码，没有运行时字节码代理。
// 当前实现直接使用 OTel API 和本仓库 semconv；没有委托 contrib/otelhttp 包装 Transport。
package client

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/instrumentation/net/http/semconv"
	"go.opentelemetry.io/otelc/pkg/hook"
	"go.opentelemetry.io/otelc/pkg/runtime"
)

const (
	otelExporterPrefix  = "OTel OTLP Exporter Go"
	instrumentationName = "go.opentelemetry.io/otelc/instrumentation/net/http"
	instrumentationKey  = "NETHTTP"
	// 参数下标把 receiver 算在内：0 是 *http.Transport，1 才是 *http.Request。
	// HookContext 本身是额外提供给 hook 的桥接对象，不占原函数的参数下标。
	requestParamIndex = 1
)

var (
	logger     = runtime.Logger()
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	initOnce   sync.Once
)

// 首次真正需要插桩时读取全局 provider/propagator；SDK 的初始化由公共 runtime 负责。
// sync.Once 保证并发请求只初始化一次；因此单元测试更换全局 provider 后也要重置它。
func initInstrumentation() {
	initOnce.Do(func() {
		tracer = otel.GetTracerProvider().Tracer(
			instrumentationName,
			trace.WithInstrumentationVersion(runtime.ModuleVersion()),
		)
		propagator = otel.GetTextMapPropagator()
		logger.Info("HTTP client instrumentation initialized")
	})
}

// debugEnabled gates per-request Debug calls: slog evaluates arguments before
// checking the level, so an unguarded call pays for req.URL.String() and
// attribute boxing on every request even when debug logging is off.
func debugEnabled() bool {
	return logger.Enabled(context.Background(), slog.LevelDebug)
}

// netHttpClientEnabler controls whether client instrumentation is enabled
type netHttpClientEnabler struct{}

func (n netHttpClientEnabler) Enable() bool {
	// runtime 会统一处理大小写、SDK 总开关和组件允许/禁用列表。
	// 环境变量中使用 nethttp；HTTP client/server 共用这一组件键，禁用列表优先。
	return runtime.Instrumented(instrumentationKey)
}

var clientEnabler = netHttpClientEnabler{}

// hookData carries span state from BeforeRoundTrip to AfterRoundTrip. A typed
// struct instead of SetKeyData's map[string]interface{} keeps the per-request
// cost to a single small allocation with no map or string hashing.
type hookData struct {
	span  trace.Span
	start time.Time
}

// ictx 只对应这一次原函数调用；req.Context() 则把 Span 传播给下游业务和网络请求。
// transport 是被拦截方法的 receiver，本实现无需改动它，也不跳过原始 RoundTrip。
func BeforeRoundTrip(ictx hook.HookContext, transport *http.Transport, req *http.Request) {
	// This runs once per outbound request; keep the disabled path free of logging.
	if !clientEnabler.Enable() {
		return
	}

	if req == nil {
		return
	}

	// 上层组件可以在 context 中标记抑制，避免同一请求在多层客户端重复建 Span。
	if runtime.IsHTTPClientInstrumentationSuppressed(req.Context()) {
		return
	}

	// Filter out OTel exporter requests to prevent infinite loops
	// 导出 Span 也会发 HTTP 请求；如果再为导出请求建 Span，就可能触发自我导出循环。
	ua := req.Header.Get("User-Agent")
	if strings.HasPrefix(ua, otelExporterPrefix) || strings.HasPrefix(ua, "OTel Go OTLP") ||
		strings.HasPrefix(ua, "OTel-Go-OTLP") {
		if debugEnabled() {
			logger.Debug("Skipping OTel exporter request", "user_agent", ua)
		}
		return
	}

	initInstrumentation()

	if debugEnabled() {
		var urlStr string
		if req.URL != nil {
			urlStr = req.URL.String()
		}
		logger.Debug("BeforeRoundTrip called",
			"method", req.Method,
			"url", urlStr,
			"host", req.Host)
	}

	ctx := req.Context()

	// Get trace attributes from semconv
	// 属性提取集中在 semconv，包含方法标准化和 URL 敏感字段脱敏；别在 hook 中另写一套。
	attrs := semconv.HTTPClientRequestTraceAttrs(req)

	// Start span
	// Start 返回的新 ctx 携带新建的 CLIENT Span；父 Span 来自请求原有的 context。
	ctx, span := tracer.Start(ctx,
		semconv.HTTPClientSpanName(req.Method),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)

	// RoundTrip must not modify the caller's request, and WithContext only
	// shallow-copies it, so the Header map would still be the caller's. That map
	// is often shared (a base header set assigned to many requests), so injecting
	// into it from concurrent calls races. Give the outgoing request its own map.
	newReq := req.WithContext(ctx)
	if req.Header == nil {
		newReq.Header = make(http.Header)
	} else {
		newReq.Header = req.Header.Clone()
	}

	// Inject trace context into the outgoing request's headers
	propagator.Inject(ctx, propagation.HeaderCarrier(newReq.Header))

	ictx.SetParam(requestParamIndex, newReq)

	// Store data for after hook
	// SetParam 才把副本交给接下来的原方法；只在局部给 req 赋值不会修改原方法实参。
	// SetData 用一个有类型的对象携带出口状态。SetKeyData 是同一底层槽位的另一种视图，
	// 两者混用会互相覆盖，不能把它们当成两份独立存储。
	ictx.SetData(&hookData{
		span:  span,
		start: time.Now(),
	})
}

// 出口 hook 的 res/err 对应 RoundTrip 的两个返回值，而不是入口的 receiver/req。
// 这里仅观察返回值，不调用 SetReturnVal，调用方收到的响应与 error 不会被替换。
func AfterRoundTrip(ictx hook.HookContext, res *http.Response, err error) {
	data, ok := ictx.GetData().(*hookData)
	// 入口可能因禁用、nil 请求、抑制或 exporter 过滤而直接返回，出口必须容忍没有状态。
	if !ok || data == nil || data.span == nil {
		logger.Debug("AfterRoundTrip: no span from before hook")
		return
	}
	span := data.span
	// Span 在 RoundTrip 返回时结束。本实现不包装/读取/关闭 res.Body，
	// 因而不覆盖调用方随后读 body 的耗时、读错误或实际响应体字节数。
	// 出口也不重新检查开关：只要入口创建了 Span，即使配置变化也必须把它结束。
	defer span.End()

	// Add response attributes
	if res != nil {
		attrs := semconv.HTTPClientResponseTraceAttrs(res)
		span.SetAttributes(attrs...)

		// Set span status based on status code
		// client 的 4xx/5xx 都是错误状态；这与 server 仅把 5xx 视为错误不同。
		code, desc := semconv.HTTPClientStatus(res.StatusCode)
		if code != codes.Unset {
			span.SetStatus(code, desc)
		}

		if debugEnabled() {
			var method, urlStr string
			if res.Request != nil {
				method = res.Request.Method
				if res.Request.URL != nil {
					urlStr = res.Request.URL.String()
				}
			}
			logger.Debug("AfterRoundTrip called",
				"method", method,
				"url", urlStr,
				"status_code", res.StatusCode,
				"duration_ms", time.Since(data.start).Milliseconds())
		}
	}

	// Handle error
	// HTTP 状态错误与 Go error 是两条路径：收到 404 可 err == nil，网络失败则可能 res == nil。
	// RecordError 添加 exception 事件，SetStatus 才设置 Span 错误状态，两者用途不同。
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(semconv.HTTPClientErrorType(err))
		logger.Debug("AfterRoundTrip called with error", "error", err)
	}
}
