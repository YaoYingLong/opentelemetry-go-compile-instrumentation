// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package v9

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/instrumentation/github.com/redis/go-redis/v9/semconv"
	"go.opentelemetry.io/otelc/pkg/runtime"
)

var (
	logger   = runtime.Logger()
	tracer   trace.Tracer
	initOnce sync.Once
)

const (
	redisAuthCmd         = "auth"
	redisHelloCmd        = "hello"
	redisSetNameOption   = "setname"
	redisHelloAuthArgN   = 2
	redisQueryTextRedact = "?"
)

func initInstrumentation() {
	initOnce.Do(func() {
		tracer = otel.GetTracerProvider().Tracer(
			instrumentationName,
			trace.WithInstrumentationVersion(runtime.ModuleVersion()),
		)
		logger.Info("Redis v9 client instrumentation initialized")
	})
}

// 中文导读：此类型实现 go-redis 的 Hook 协议；AddHook 安装后，由库在命令执行时调用。
// 它是函数包装器模式：返回的新函数在 next 前后做追踪，next 负责真实命令执行。
// 不需要 HookContext.SetData，因为闭包和 defer 已覆盖整个命令的同步调用生命周期。
type otelRedisHook struct {
	Addr string
}

func newOtelRedisHook(addr string) *otelRedisHook {
	return &otelRedisHook{
		Addr: addr,
	}
}

func (o *otelRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		// 即使构造 client 时已注册 Hook，运行时禁用也必须继续 next，保证业务命令执行。
		if !redisEnabler.Enable() {
			logger.Debug("Redis Client instrumentation disabled")
			return next(ctx, cmd)
		}
		initInstrumentation()
		fullName := cmd.FullName()
		// 命令名可包含子命令；属性拼装集中在 semconv，Span 命名与 db.operation.name 复用它。
		request := semconv.RedisRequest{
			Endpoint:  o.Addr,
			FullName:  fullName,
			Statement: getRedisV9Statement(cmd),
		}
		// Get trace attributes from semconv
		attrs := semconv.RedisClientRequestTraceAttrs(request)

		// Start span
		spanName := request.FullName
		ctx, span := tracer.Start(ctx,
			spanName,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)
		defer span.End()

		// 把带有新 Span 的 ctx 传给 next；若仍传旧 ctx，命令内部的下游调用就看不到这个父节点。
		err := next(ctx, cmd)
		// redis.Nil 表示不存在的 key 等空结果：仍原样返回给业务，但不标记 Span 为错误。
		// 其余 error 同时记录 exception 事件和 Error 状态；记录事件本身不会自动设置状态。
		if err != nil && !errors.Is(err, redis.Nil) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
}

func (o *otelRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !redisEnabler.Enable() {
			logger.Debug("Redis Client instrumentation disabled")
			return next(ctx, cmds)
		}
		initInstrumentation()

		// pipeline 在此只建一个汇总 Span，不为本函数内的每条命令分别建 Span。
		// 最多取前 10 个命令名组成查询摘要，超出追加 ...，避免随批量长度无限增长。
		summary := ""
		summaryCmds := cmds
		if len(summaryCmds) > 10 {
			summaryCmds = summaryCmds[:10]
		}
		for i := range summaryCmds {
			summary += summaryCmds[i].FullName() + "/"
		}
		if len(cmds) > 10 {
			summary += "..."
		}
		cmd := redis.NewCmd(ctx, "pipeline", summary)
		// 这个合成 Cmder 只用于生成 telemetry；真正执行的仍是下面 next(ctx, cmds) 的原列表。
		fullName := cmd.FullName()
		request := semconv.RedisRequest{
			Endpoint:  o.Addr,
			FullName:  fullName,
			Statement: getRedisV9Statement(cmd),
		}

		// Get trace attributes from semconv
		attrs := semconv.RedisClientRequestTraceAttrs(request)

		// Start span
		spanName := request.FullName
		ctx, span := tracer.Start(ctx,
			spanName,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attrs...),
		)
		defer span.End()

		err := next(ctx, cmds)
		if err != nil && !errors.Is(err, redis.Nil) {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
}

func (o *otelRedisHook) DialHook(next redis.DialHook) redis.DialHook {
	// 为满足 go-redis Hook 接口提供连接包装；当前仅透传连接/错误，没有额外创建拨号 Span。
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := next(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return conn, err
	}
}

func getRedisV9Statement(cmd redis.Cmder) string {
	// db.query.text 来源于 Args 的可读序列化。AUTH/HELLO AUTH 的凭证单独脱敏，
	// 普通 SET/GET 的 key/value 仍可能出现在属性中；不要把它误解为所有业务数据均已脱敏。
	// 该函数在 next 之前调用，此时 cmd.Err() 若已有值才追加，并非执行结束后重取查询文本。
	args := cmd.Args()
	redactStart, redactEnd := redisV9CredentialRedactRange(cmd.Name(), args)

	b := make([]byte, 0, 64)
	for i, arg := range args {
		if i > 0 {
			b = append(b, ' ')
		}
		if i >= redactStart && i < redactEnd {
			b = append(b, redisQueryTextRedact...)
			continue
		}
		b = redisV9AppendArg(b, arg)
	}

	if err := cmd.Err(); err != nil && !errors.Is(err, redis.Nil) {
		b = append(b, ": "...)
		b = append(b, err.Error()...)
	}

	return string(b)
}

// redisV9CredentialRedactRange returns a half-open index range of args that
// must not appear in db.query.text. AUTH arguments are credentials. HELLO
// AUTH username/password are too; SETNAME and the protocol version are not.
func redisV9CredentialRedactRange(name string, args []interface{}) (start, end int) {
	// 返回半开区间 [start,end)，空区间表示无需隐藏；脱敏只修改展示文本，不修改原命令参数。
	switch name {
	case redisAuthCmd:
		if len(args) > 1 {
			return 1, len(args)
		}
	case redisHelloCmd:
		if i := redisV9HelloAuthIndex(args); i >= 0 {
			return i + 1, min(i+1+redisHelloAuthArgN, len(args))
		}
	}
	return 0, 0
}

func redisV9HelloAuthIndex(args []interface{}) int {
	for i := 1; i < len(args); i++ {
		if !redisV9ArgEqualFold(args[i], redisAuthCmd) {
			continue
		}
		// AUTH and SETNAME each take a fixed number of args in the HELLO
		// grammar, so a client literally named "auth" can only appear
		// directly after "setname" - a one-token lookback is enough.
		if i > 1 && redisV9ArgEqualFold(args[i-1], redisSetNameOption) {
			continue
		}
		return i
	}
	return -1
}

func redisV9ArgEqualFold(v interface{}, s string) bool {
	switch a := v.(type) {
	case string:
		return strings.EqualFold(a, s)
	case []byte:
		return strings.EqualFold(string(a), s)
	default:
		return false
	}
}

func redisV9AppendArg(b []byte, v interface{}) []byte {
	// 使用类型分支追加到 byte slice，控制分配；非 UTF-8 字符串/字节仅输出占位文本，
	// 避免把非法 UTF-8 写入遥测属性。不支持的类型也只影响展示，不影响传给 Redis 的值。
	switch v := v.(type) {
	case nil:
		return append(b, "<nil>"...)
	case string:
		if utf8.ValidString(v) {
			return append(b, v...)
		}
		return append(b, "<string>"...)
	case []byte:
		if utf8.Valid(v) {
			return append(b, v...)
		}
		return append(b, "<byte>"...)
	case int:
		return strconv.AppendInt(b, int64(v), 10)
	case int8:
		return strconv.AppendInt(b, int64(v), 10)
	case int16:
		return strconv.AppendInt(b, int64(v), 10)
	case int32:
		return strconv.AppendInt(b, int64(v), 10)
	case int64:
		return strconv.AppendInt(b, v, 10)
	case uint:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint8:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint16:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint32:
		return strconv.AppendUint(b, uint64(v), 10)
	case uint64:
		return strconv.AppendUint(b, v, 10)
	case float32:
		return strconv.AppendFloat(b, float64(v), 'f', -1, 64)
	case float64:
		return strconv.AppendFloat(b, v, 'f', -1, 64)
	case bool:
		if v {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case time.Time:
		return v.AppendFormat(b, time.RFC3339Nano)
	default:
		return append(b, "not_support_type"...)
	}
}
