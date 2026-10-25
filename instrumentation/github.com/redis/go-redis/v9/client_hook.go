// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 中文导读：Redis 采用“编译时挂接入口 + 库原生 Hook 执行命令”的两层结构。
// otelc.yaml 在构造函数返回后调用本文件；这里把自实现 otelRedisHook 注册到 go-redis。
// 每条命令真正开始/结束 Span 的逻辑在 hook.go，并非调用 redisotel.InstrumentTracing。
package v9

import (
	"context"

	redis "github.com/redis/go-redis/v9"

	"go.opentelemetry.io/otelc/pkg/hook"
	"go.opentelemetry.io/otelc/pkg/runtime"
)

const (
	instrumentationName = "go.opentelemetry.io/otelc/instrumentation/github.com/redis/go-redis/v9"
	instrumentationKey  = "REDIS"
)

// redisClientEnabler controls whether client instrumentation is enabled
type redisClientEnabler struct{}

func (g redisClientEnabler) Enable() bool {
	// 环境变量中组件名用 redis，与 HTTP 的 nethttp 不同；解析和禁用优先级由 runtime 处理。
	return runtime.Instrumented(instrumentationKey)
}

var redisEnabler = redisClientEnabler{}

// NewClient 是普通函数，出口 hook 的 client 来自其返回值，不是构造函数的 Options 入参。
// 此处不必 SetReturnVal：对返回对象 AddHook 后，业务继续拿同一个 client 执行命令。
// 构造阶段总会注册 Hook；是否启用追踪由 ProcessHook/ProcessPipelineHook 在调用时判断。
func afterNewRedisClientV9(ictx hook.HookContext, client *redis.Client) {
	client.AddHook(newOtelRedisHook(client.Options().Addr))
}

func afterNewFailOverRedisClientV9(call hook.HookContext, client *redis.Client) {
	// Failover 返回普通 *redis.Client，因此可复用相同命令 Hook；端点来自 client.Options()。
	client.AddHook(newOtelRedisHook(client.Options().Addr))
}

func afterNewRingClientV9(call hook.HookContext, client *redis.Ring) {
	// Ring 不能只在最外层挂一个 Hook：需要给每个分片的具体 *redis.Client 安装。
	// OnNewNode 覆盖后续 SetAddrs 动态新增的节点，下面再补装构造时已创建的分片。
	client.OnNewNode(func(rdb *redis.Client) {
		rdb.AddHook(newOtelRedisHook(rdb.Options().Addr))
	})
	// NewRing creates the configured shards before this hook runs, so
	// OnNewNode never sees them.
	attachHookToExistingShards(client)
}

func attachHookToExistingShards(client *redis.Ring) {
	// 仅注册回调不能追溯已存在的节点；单元测试分别断言现有节点和未来节点被覆盖。
	_ = client.ForEachShard(context.Background(), func(_ context.Context, rdb *redis.Client) error {
		rdb.AddHook(newOtelRedisHook(rdb.Options().Addr))
		return nil
	})
}

// Cluster nodes are created lazily, after this callback is registered, so
// OnNewNode alone covers them.
func afterNewClusterClientV9(call hook.HookContext, client *redis.ClusterClient) {
	// 与 Ring 的立即创建不同，Cluster 节点延迟创建，因此先注册回调即可覆盖。
	client.OnNewNode(func(rdb *redis.Client) {
		rdb.AddHook(newOtelRedisHook(rdb.Options().Addr))
	})
}

func afterNewSentinelClientV9(call hook.HookContext, client *redis.SentinelClient) {
	// 这里传入的是 String()，并非 Options().Addr；最终 server.address 可能是完整描述字符串。
	client.AddHook(newOtelRedisHook(client.String()))
}

func afterClientConnV9(call hook.HookContext, client *redis.Conn) {
	// 只有 v9.0.0 <= version < v9.9.0 的 Conn 才匹配此规则；新版继承客户端 Hook，
	// 再手工注册会让一次 Conn GET 产生重复 Span。版本边界由 otelc.yaml 控制。
	client.AddHook(newOtelRedisHook(client.String()))
}
