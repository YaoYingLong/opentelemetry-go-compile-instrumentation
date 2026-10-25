// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 中文导读：规则拦截 net/http 内部 serverHandler.ServeHTTP，而非逐个改写业务 Handler。
// 本包直接建 SERVER Span，再通过请求 context 交给实际路由/业务处理器。
// 配套 response_writer.go 只包装响应写入接口来观察状态码，并未使用 contrib/otelhttp。
package server

import (
	"context"
	"log/slog"
	"net/http"
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
	instrumentationName = "go.opentelemetry.io/otelc/instrumentation/net/http"
	instrumentationKey  = "NETHTTP"
	// 方法参数下标包含 receiver：0=serverHandler，1=ResponseWriter，2=*Request。
	// 入口 hook 的 ictx 是额外提供的工具参数，不属于这个下标序列。
	responseWriterIndex = 1
	requestIndex        = 2
)

var (
	logger     = runtime.Logger()
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	initOnce   sync.Once
)

// 复用公共 runtime 安装好的全局 provider/propagator，首次请求时只初始化一次。
func initInstrumentation() {
	initOnce.Do(func() {
		tracer = otel.GetTracerProvider().Tracer(
			instrumentationName,
			trace.WithInstrumentationVersion(runtime.ModuleVersion()),
		)
		propagator = otel.GetTextMapPropagator()
		logger.Info("HTTP server instrumentation initialized")
	})
}

// debugEnabled gates per-request Debug calls: slog evaluates arguments before
// checking the level, so an unguarded call pays for r.URL.String() and
// attribute boxing on every request even when debug logging is off.
func debugEnabled() bool {
	return logger.Enabled(context.Background(), slog.LevelDebug)
}

// netHttpServerEnabler controls whether server instrumentation is enabled
type netHttpServerEnabler struct{}

func (n netHttpServerEnabler) Enable() bool {
	// 与 HTTP client 共用 nethttp 开关；环境变量解析和禁用优先级由 runtime 统一实现。
	return runtime.Instrumented(instrumentationKey)
}

var serverEnabler = netHttpServerEnabler{}

// hookData carries span state from BeforeServeHTTP to AfterServeHTTP. A typed
// struct instead of SetKeyData's map[string]interface{} keeps the per-request
// cost to a single small allocation with no map or string hashing.
type hookData struct {
	span  trace.Span
	start time.Time
}

// recv 用 interface{} 接住 net/http 的非导出 receiver 类型，避免本包直接引用该类型。
// 对照 Java Advice：HookContext 是一次调用的状态桥，而 r.Context() 是跨组件传播的业务上下文。
func BeforeServeHTTP(ictx hook.HookContext, recv interface{}, w http.ResponseWriter, r *http.Request) {
	// This runs once per request; keep the disabled path free of logging.
	if !serverEnabler.Enable() {
		return
	}

	initInstrumentation()

	if debugEnabled() {
		logger.Debug("BeforeServeHTTP called",
			"method", r.Method,
			"url", r.URL.String(),
			"remote_addr", r.RemoteAddr)
	}

	// Extract trace context from incoming request headers
	// 提取远端传播头，使新建 Span 成为上游请求 Span 的子节点；不是另起一个无关 trace。
	ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

	// Get trace attributes from semconv
	attrs := semconv.HTTPServerRequestTraceAttrs("", r)

	// Route isn't known until ServeMux matches. AfterServeHTTP renames the span.
	// 此处尚未进入 ServeMux，先用方法名；出口再用 /users/{id} 这类模板，避免真实路径的高基数。
	spanName := semconv.HTTPServerSpanName(r.Method, "")

	// Start span
	ctx, span := tracer.Start(ctx,
		spanName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	)

	// Wrap ResponseWriter to capture status code
	// ServeHTTP 没有返回值，状态码只能从 w.WriteHeader/Write 等写入路径观察。
	// 仅给局部变量 w 赋值不够，SetParam 才会让原始 ServeHTTP 使用这个包装器。
	wrapper := &writerWrapper{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
	}
	ictx.SetParam(responseWriterIndex, wrapper)

	// Update request with new context containing the span
	// WithContext 返回浅拷贝；入口更换请求参数后，业务处理器从 r.Context() 即可取得当前 Span。
	newReq := r.WithContext(ctx)
	ictx.SetParam(requestIndex, newReq)

	// Store data for after hook
	// 此状态只属于一次 ServeHTTP 调用，不能放在包级全局变量中，否则并发请求会串 Span。
	// SetData 与 SetKeyData 共用存储槽，本包始终用有类型的 hookData，避免混用覆盖。
	ictx.SetData(&hookData{
		span:  span,
		start: time.Now(),
	})
}

// 原函数不返回 error，所以出口只取回入口状态和修改后的参数，并依据 HTTP 状态结束 Span。
// 这里没有 recover/RecordError 逻辑，不能把它理解成已覆盖所有业务 panic 或写 body 错误。
func AfterServeHTTP(ictx hook.HookContext) {
	data, ok := ictx.GetData().(*hookData)
	if !ok || data == nil || data.span == nil {
		logger.Debug("AfterServeHTTP: no span from before hook")
		return
	}
	span := data.span
	defer span.End()

	// ServeMux fills in r.Pattern on the same request after the span was created.
	// Stays empty for routers that name the span themselves (gin, chi).
	// 从 ictx 取到的是入口替换后的请求，才能看到 ServeMux 随后填入的 Pattern。
	if r, ok := ictx.GetParam(requestIndex).(*http.Request); ok && r != nil && span.IsRecording() {
		if route := semconv.HTTPRoute(r.Pattern); route != "" {
			span.SetName(semconv.HTTPServerSpanName(r.Method, route))
			span.SetAttributes(semconv.HTTPServerRoute(route))
		}
	}

	// Extract status code from wrapped ResponseWriter
	statusCode := http.StatusOK
	if p, ok := ictx.GetParam(responseWriterIndex).(http.ResponseWriter); ok {
		if wrapper, ok := p.(*writerWrapper); ok {
			statusCode = wrapper.statusCode
		}
	}

	// Add response attributes
	// 第二个实参是写入字节数；当前固定 0，wrapper 没有统计 body 大小或读取 body 内容。
	attrs := semconv.HTTPServerResponseTraceAttrs(statusCode, 0)
	span.SetAttributes(attrs...)

	// Set span status based on status code
	// server 4xx 保持 Unset，5xx 标为 Error；请勿照抄 HTTP client 的 4xx 错误策略。
	code, desc := semconv.HTTPServerStatus(statusCode)
	if code != codes.Unset {
		span.SetStatus(code, desc)
	}

	if debugEnabled() {
		logger.Debug("AfterServeHTTP called",
			"status_code", statusCode,
			"duration_ms", time.Since(data.start).Milliseconds())
	}
}
