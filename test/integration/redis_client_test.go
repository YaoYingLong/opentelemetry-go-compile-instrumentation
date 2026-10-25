// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otelc/test/testutil"
)

const redisGoRedisModule = "github.com/redis/go-redis/v9"

// 中文导读：不仅验证 AddHook 本身，还用 otelc 构建真实 app，验证 YAML 的构造函数匹配。
// 普通单元测试直接调用 afterClientConnV9，无法证明该 hook 在哪些依赖版本上应当被注入。
// 本测试同时跑旧版/新版，专门防止版本边界错误造成 Conn Span 缺失或重复。
func TestRedisClient(t *testing.T) {
	t.Parallel()

	// Copy before either build. otelc setup mutates test/apps/redisclient/go.mod,
	// and a parallel copy would pick up the instrumentation require and MVS
	// to v9.22.0.
	legacyAppsDir := pinnedRedisClient(t, "v9.8.0")
	// 复制和 pin 必须先于两条并行构建：otelc setup 会修改 app 的依赖，不能边改边复制。

	// Both cases go through otelc. Equal GET counts fail if the Conn version
	// gate is wrong: missing the pre-v9.9 hook drops a span, injecting the
	// hook after v9.9.0 adds a duplicate.
	for _, tc := range []struct {
		name    string
		appsDir string
		version string
	}{
		{name: "conn_after_v9.9"},
		{name: "conn_v9.8.0", appsDir: legacyAppsDir, version: "v9.8.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			testutil.Build(t, tc.appsDir, "redisclient", "go", "build", "-a")
			if tc.version != "" {
				requireRedisModuleVersion(t, filepath.Join(tc.appsDir, "redisclient"), tc.version)
			}

			assertRedisClientSpans(t, tc.appsDir)
		})
	}
}

func assertRedisClientSpans(t *testing.T, appsDir string) {
	t.Helper()

	var opts []testutil.TestFixtureOption
	if appsDir != "" {
		opts = append(opts, testutil.WithAppsDir(appsDir))
	}
	f := testutil.NewTestFixture(t, opts...)
	// miniredis 是进程内 Redis 协议服务，无需 Docker；实际命令仍来自插桩后的业务子进程。
	server := StartRedisServer(t)

	output := f.Run("redisclient", "-addr="+server.Addr())
	// 输出断言验证业务仍读到正确值；后续断言验证真实导出的遥测，不能以日志代替 Span 校验。
	require.Contains(t, output, "testvalue")

	spans := testutil.AllSpans(f.Traces())
	require.GreaterOrEqual(t, len(spans), 4, "expected at least 4 spans (SET, GET, CONN GET, DEL)")
	// 允许握手等附带 Span，所以总数设下界；核心 GET 操作在下面单独要求精确为 2，避免放过重复。

	setSpan := testutil.RequireSpan(t, f.Traces(),
		// 以 CLIENT 类型和 db.operation.name 选择业务 Span，不依赖 OTLP 到达顺序。
		testutil.IsClient,
		testutil.HasAttribute("db.operation.name", "set"),
	)
	testutil.RequireRedisClientSemconv(
		t,
		setSpan,
		"set",
		server.Addr(),
		"set testkey testvalue",
	)

	host, _, err := net.SplitHostPort(server.Addr())
	require.NoError(t, err)

	getCount := 0
	clientGet := false
	for _, span := range spans {
		if !testutil.IsClient(span) || !testutil.HasAttribute("db.operation.name", "get")(span) {
			continue
		}
		getCount++
		// Pre-v9.9 Conn hooks store Conn.String() as the endpoint, so only the
		// NewClient GET is guaranteed to have server.address == host.
		// 旧版 Conn 的 String() 不是标准 host:port，不能强行对它复用普通 Client 的端点断言。
		if testutil.HasAttribute("server.address", host)(span) {
			clientGet = true
			testutil.RequireRedisClientSemconv(
				t,
				span,
				"get",
				server.Addr(),
				"get testkey",
			)
		}
	}
	require.Equal(t, 2, getCount, "client GET and Conn GET should each produce one span")
	// 旧版漏注入 Conn Hook 时只有 1 个 GET；新版多注入一次时会出现第 3 个，均在这里失败。
	require.True(t, clientGet, "NewClient GET span is missing")

	delSpan := testutil.RequireSpan(t, f.Traces(),
		testutil.IsClient,
		testutil.HasAttribute("db.operation.name", "del"),
	)
	testutil.RequireRedisClientSemconv(
		t,
		delSpan,
		"del",
		server.Addr(),
		"del testkey",
	)
}

// pinnedRedisClient copies redisclient and pins go-redis so otelc matches the
// Conn rule against that module version, not the committed go.mod.
func pinnedRedisClient(t *testing.T, version string) string {
	t.Helper()

	src, err := filepath.Abs(filepath.Join("..", "apps", "redisclient"))
	require.NoError(t, err)

	appsDir := t.TempDir()
	appDir := filepath.Join(appsDir, "redisclient")
	require.NoError(t, os.MkdirAll(appDir, 0o755))
	require.NoError(t, os.CopyFS(appDir, os.DirFS(src)))

	// Replace first so tidy cannot MVS up to the instrumentation module pin.
	// Go 的最小版本选择可能把显式 go get 的旧版提升到 hook 模块所需的新版本。
	// 临时 app 用 replace 锁定旧版，再 go list 断言实际版本，保证测到的确实是目标边界。
	goModOffWork(t, appDir, "edit", "-replace="+redisGoRedisModule+"="+redisGoRedisModule+"@"+version)
	cmd := exec.CommandContext(t.Context(), "go", "get", redisGoRedisModule+"@"+version)
	cmd.Dir = appDir
	cmd.Env = goWorkOffEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go get %s@%s:\n%s", redisGoRedisModule, version, out)
	goModOffWork(t, appDir, "tidy")
	requireRedisModuleVersion(t, appDir, version)
	return appsDir
}

func requireRedisModuleVersion(t *testing.T, appDir, version string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Version}}", redisGoRedisModule)
	cmd.Dir = appDir
	cmd.Env = goWorkOffEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "go list -m %s failed in %s", redisGoRedisModule, appDir)
	require.Equal(t, version, strings.TrimSpace(string(out)),
		"%s must stay at %s so the Conn version gate is the one under test", redisGoRedisModule, version)
}

func goModOffWork(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"mod"}, args...)...)
	cmd.Dir = dir
	cmd.Env = goWorkOffEnv()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func goWorkOffEnv() []string {
	// 关闭仓库 workspace，避免临时 app 的依赖被根 go.work 中的其他模块影响。
	return append(os.Environ(), "GOWORK=off")
}

// StartRedisServer creates and starts a miniredis server for testing.
// The server is automatically closed when the test completes.
func StartRedisServer(t *testing.T) *miniredis.Miniredis {
	s, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(s.Close)
	return s
}
