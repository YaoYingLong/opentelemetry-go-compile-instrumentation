// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 本包是应用运行时的公共支持层，不是 Go 标准库 runtime。
// 阅读顺序：SetupOTelSDK -> Initialize -> setupOpenTelemetry -> 各 Provider。
// 编译工具负责把调用入口放进应用；本文件的函数在生成的应用启动/运行时执行。
package runtime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	"go.opentelemetry.io/otel"
	logglobal "go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	// Default export intervals and batch sizes
	defaultTraceBatchTimeout = time.Second
	defaultTraceBatchSize    = 512
)

var (
	logger                *slog.Logger
	meterProvider         *sdkmetric.MeterProvider
	tracerProvider        *sdktrace.TracerProvider
	loggerProvider        *sdklog.LoggerProvider
	registerSignalHandler sync.Once
)

func init() {
	// Go 在调用包的使用方之前完成依赖包初始化。这里先准备日志器；
	// SDK 初始化入口则位于注入 main 包的 init_otelsdk.go，两者职责不同。
	// Initialize logger early so hook packages can use it with the correct log level
	// This is called at package load time, before any hooks execute
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel(),
	}))
}

// Config holds configuration for OpenTelemetry setup
type Config struct {
	InstrumentationName    string
	InstrumentationVersion string
}

// Initialize sets up OpenTelemetry with defensive error handling
func Initialize(cfg Config) {
	// defer 注册当前函数返回/展开 panic 时要运行的闭包。
	// recover 必须在延迟调用中捕获当前 goroutine 的 panic；它不是全局异常处理器。
	// Defensive: ensure instrumentation initialization never crashes user application
	defer func() {
		if rec := recover(); rec != nil {
			// Log panic but don't propagate - user application must continue
			Logger().Error("panic during OpenTelemetry initialization", "panic", rec)
		}
	}()

	// Setup OpenTelemetry
	setupOpenTelemetry(cfg)

	// Setup automatic shutdown on signals
	setupSignalHandler()
}

// Logger returns the package logger
func Logger() *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger
}

// logLevel returns the log level from environment variable
func logLevel() slog.Level {
	levelStr := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_LOG_LEVEL")))
	switch levelStr {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newResource builds the process resource for auto-configured providers.
// WithTelemetrySDK stamps telemetry.sdk.name/language/version, which
// resource.Default() includes but resource.New() does not unless asked.
// WithService stamps service.name (unknown_service:<executable>) and
// service.instance.id. WithFromEnv stays last so OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES still win.
func newResource(ctx context.Context) *resource.Resource {
	// Resource 描述产生遥测的服务/进程，Provider 持有它；每次请求的 URL、
	// 状态码等属于 Span 属性，不应混入这里。多个 WithXxx 是函数选项模式。
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithService(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithHost(),
		resource.WithFromEnv())
	if err != nil {
		logger.Warn("failed to create resource", "error", err)
		return resource.Default()
	}
	return res
}

// setupOpenTelemetry initializes the OpenTelemetry SDK with OTLP exporters
func setupOpenTelemetry(cfg Config) {
	// Defensive: catch any panics during setup
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error("panic during OpenTelemetry setup", "panic", rec)
		}
	}()

	// The default handler writes to the stdlib logger on stderr, which bypasses OTEL_LOG_LEVEL and does not match
	// the structured output the rest of the runtime emits.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Error("OpenTelemetry SDK error", "error", err)
	}))

	ctx := context.Background()
	// 这里的 ctx 用于初始化，不是请求上下文。三个信号各自安装 Provider，
	// 一个返回 error 时记录日志并继续尝试其他信号，不让普通配置失败中断应用。
	res := newResource(ctx)

	// Setup trace provider with auto-configured exporter
	if err := setupTraceProvider(ctx, res); err != nil {
		logger.Warn("failed to setup trace provider", "error", err)
	}

	// Setup meter provider with auto-configured exporter
	if err := setupMeterProvider(ctx, res); err != nil {
		logger.Warn("failed to setup meter provider", "error", err)
	}

	// Setup logger provider with auto-configured exporter
	if err := setupLoggerProvider(ctx, res); err != nil {
		logger.Warn("failed to setup logger provider", "error", err)
	}

	// Use autoprop to select propagators from OTEL_PROPAGATORS (tracecontext,
	// baggage, b3, b3multi, jaeger, xray, ottrace, none). Defaults to W3C
	// Trace Context + Baggage when the variable is not set.
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator())

	logger.Info("OpenTelemetry initialized",
		"instrumentation_name", cfg.InstrumentationName,
		"instrumentation_version", cfg.InstrumentationVersion)
}

// setupTraceProvider creates and configures the trace provider
func setupTraceProvider(ctx context.Context, res *resource.Resource) error {
	// Use autoexport to automatically select the right exporter based on
	// OTEL_TRACES_EXPORTER (defaults to otlp) and OTEL_EXPORTER_OTLP_PROTOCOL
	// (defaults to http/protobuf). Supports: otlp, console, and none.
	//
	// This mirrors setupMeterProvider: the exporter-selection env vars decide
	// whether/where traces go, not the presence of an OTLP endpoint env var.
	// When otlp is selected (the default) and no endpoint is configured, the
	// exporter falls back to the OTLP-spec default endpoint.
	traceExporter, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		return err
	}

	// OTEL_TRACES_EXPORTER=none: skip building a provider/processor entirely
	// rather than running a batch processor that will never export anything.
	if autoexport.IsNoneSpanExporter(traceExporter) {
		logger.Debug("trace exporter disabled via OTEL_TRACES_EXPORTER=none, skipping trace provider setup")
		return nil
	}

	spanProcessor := newSpanProcessor(traceExporter)
	// 职责链：Tracer 创建 Span -> SpanProcessor 处理结束的 Span -> Exporter 导出。
	// Provider 则统一管理 Tracer、Resource 和处理器；这与 Java OTel SDK 的分层相近。

	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(spanProcessor),
	)

	// Set global tracer provider
	otel.SetTracerProvider(tracerProvider)

	logger.Info("trace provider initialized with auto-export")
	return nil
}

func newSpanProcessor(traceExporter sdktrace.SpanExporter) sdktrace.SpanProcessor {
	// Simple 在 Span 结束时同步导出，便于短程序/测试观察；Batch 通过缓冲批量导出。
	// “编译时插桩”仍有 Span 创建和导出的运行时成本，不能理解为遥测零开销。
	if useSimpleSpanProcessor() {
		logger.Debug("using SimpleSpanProcessor for immediate span export")
		return sdktrace.NewSimpleSpanProcessor(traceExporter)
	}

	return sdktrace.NewBatchSpanProcessor(traceExporter,
		sdktrace.WithBatchTimeout(defaultTraceBatchTimeout),
		sdktrace.WithMaxExportBatchSize(defaultTraceBatchSize),
	)
}

func useSimpleSpanProcessor() bool {
	return strings.EqualFold(os.Getenv("OTEL_GO_SIMPLE_SPAN_PROCESSOR"), "true")
}

// setupMeterProvider creates and configures the meter provider
func setupMeterProvider(ctx context.Context, res *resource.Resource) error {
	// Use autoexport to automatically select the right exporter based on
	// OTEL_METRICS_EXPORTER (defaults to otlp) and OTEL_EXPORTER_OTLP_PROTOCOL
	// (defaults to http/protobuf). Supports: otlp, console, prometheus, and none.
	metricReader, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		return err
	}

	// OTEL_METRICS_EXPORTER=none: skip building a provider around a reader
	// that will never be collected, matching the traces/logs behavior.
	if autoexport.IsNoneMetricReader(metricReader) {
		logger.Debug("metric exporter disabled via OTEL_METRICS_EXPORTER=none, skipping meter provider setup")
		return nil
	}

	// Create meter provider with the auto-configured reader
	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(metricReader),
	)

	// Set global meter provider
	otel.SetMeterProvider(meterProvider)

	logger.Info("meter provider initialized with auto-export")
	return nil
}

// setupLoggerProvider creates and configures the logger provider
func setupLoggerProvider(ctx context.Context, res *resource.Resource) error {
	// Use autoexport to automatically select the right exporter based on
	// OTEL_LOGS_EXPORTER (defaults to otlp) and OTEL_EXPORTER_OTLP_PROTOCOL
	// (defaults to http/protobuf). Supports: otlp, console, and none.
	//
	// This mirrors setupTraceProvider/setupMeterProvider so all three signals
	// are configured symmetrically via autoexport.
	logExporter, err := autoexport.NewLogExporter(ctx)
	if err != nil {
		return err
	}

	// OTEL_LOGS_EXPORTER=none: skip building a provider/processor entirely
	// rather than running a batch processor that will never export anything.
	if autoexport.IsNoneLogExporter(logExporter) {
		logger.Debug("log exporter disabled via OTEL_LOGS_EXPORTER=none, skipping logger provider setup")
		return nil
	}

	loggerProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
	)

	// Set global logger provider
	logglobal.SetLoggerProvider(loggerProvider)

	logger.Info("logger provider initialized with auto-export")
	return nil
}

// Shutdown gracefully shuts down the OpenTelemetry SDK
func Shutdown(ctx context.Context) error {
	// 不在第一个错误处退出：尽量关闭三个 Provider，再聚合错误交给调用者。
	// ctx 约束这次关闭的时限；程序直接退出并不意味着所有缓冲数据都已发送。
	var errs []error

	if tracerProvider != nil {
		if shutdownErr := tracerProvider.Shutdown(ctx); shutdownErr != nil {
			Logger().Error("failed to shutdown tracer provider", "error", shutdownErr)
			errs = append(errs, shutdownErr)
		}
	}

	if meterProvider != nil {
		if shutdownErr := meterProvider.Shutdown(ctx); shutdownErr != nil {
			Logger().Error("failed to shutdown meter provider", "error", shutdownErr)
			errs = append(errs, shutdownErr)
		}
	}

	if loggerProvider != nil {
		if shutdownErr := loggerProvider.Shutdown(ctx); shutdownErr != nil {
			Logger().Error("failed to shutdown logger provider", "error", shutdownErr)
			errs = append(errs, shutdownErr)
		}
	}

	return errors.Join(errs...)
}

// StartRuntimeMetrics enables Go runtime metrics collection.
// This follows the same enable/disable pattern as other instrumentations via
// OTEL_GO_ENABLED_INSTRUMENTATIONS and OTEL_GO_DISABLED_INSTRUMENTATIONS.
//
// Runtime metrics are enabled by default. To disable:
//   - Set OTEL_GO_DISABLED_INSTRUMENTATIONS=runtimemetrics
//   - Or set OTEL_GO_ENABLED_INSTRUMENTATIONS without including "runtimemetrics"
//
// Returns error if runtime metrics fail to start, but this is non-fatal.
func StartRuntimeMetrics() {
	if !Instrumented("runtimemetrics") {
		logger.Debug("runtime metrics disabled via environment variable")
		return
	}

	// Get the meter provider from the global registry
	mp := otel.GetMeterProvider()

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		logger.Warn("failed to start runtime metrics", "error", err)
		return
	}

	logger.Info("runtime metrics enabled")
}

// shutdownSignals returns the OS signals that trigger graceful OTel shutdown.
func shutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

// setupSignalHandler flushes the OTel SDK on SIGINT/SIGTERM so buffered telemetry
// survives shutdown, then steps aside. It never exits or re-raises the signal:
// the application owns its own exit path and exit code. Terminating here would
// race an application running its own graceful shutdown and could truncate it.
func setupSignalHandler() {
	// sync.Once 保证这个信号监听器只注册一次，不代表整个 SDK 初始化只执行一次。
	// 带 1 个缓冲槽的 channel 接收 OS 信号，独立 goroutine 等待，不阻塞主程序。
	registerSignalHandler.Do(func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, shutdownSignals()...)
		go handleShutdownSignal(sigCh)
	})
}

// handleShutdownSignal waits for the first signal on sigCh and flushes the SDK.
// It does not exit or re-raise the signal, leaving the process exit to the
// application or the OS default disposition.
func handleShutdownSignal(sigCh chan os.Signal) {
	// <- 是 channel 接收操作；没有信号时此 goroutine 阻塞等待。
	sig := <-sigCh

	// Stop listening now so a repeated signal isn't swallowed by our buffered
	// channel but reaches the app or the default disposition, keeping the
	// "press again to force quit" behavior.
	signal.Stop(sigCh)

	logger.Info("received signal, flushing telemetry", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	// 即使 Shutdown 提前完成也要 cancel，及时释放计时器等资源。
	defer cancel()

	if err := Shutdown(ctx); err != nil {
		logger.Error("error flushing telemetry during shutdown", "error", err)
	} else {
		logger.Info("OpenTelemetry SDK shutdown completed successfully")
	}
}
