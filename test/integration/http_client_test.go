// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otelc/test/testutil"
)

// 中文导读：这层验证“规则匹配 → 编译注入 → 运行时 SDK → OTLP 导出”的完整链路。
// 测试进程启动本地 HTTP server 和 collector；真正发请求的是 test/apps/httpclient 子进程。
// 与 hook 单元测试不同，业务 app 没有手工调用 BeforeRoundTrip/AfterRoundTrip。
func TestHTTPClient(t *testing.T) {
	t.Parallel()
	// Build 实际通过 otelc go build 构建并复用业务二进制；-a 强制重编译以经过插桩流程。
	// 若配置 OTELC_TEST_GOCACHE，公共 helper 会去掉 -a，改用该 app 独立的构建缓存。
	testutil.Build(t, "", "httpclient", "go", "build", "-a")

	testCases := []struct {
		name       string
		queryParam string
	}{
		{
			name:       "basic",
			queryParam: "world",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewTestFixture(t)
			// fixture 启动接收 OTLP HTTP/protobuf 的 collector，并只给子进程设置导出环境。
			server := StartHTTPServerWithResponse(t, 200, `{"message":"Hello"}`)

			f.Run("httpclient", "-addr="+server.URL, "-name="+tc.queryParam)

			span := f.RequireSingleSpan()
			// 单纯断言“程序能跑”不够：这里既检查唯一 Span，又核对方法、URL、地址、状态和协议。
			// 若完全未注入会没有 Span；若导出请求也被追踪，或入口重复插桩，则数量也会失败。
			expectedURL := server.URL + "/hello?name=" + tc.queryParam
			testutil.RequireHTTPClientSemconv(
				t,
				span,
				"GET",
				expectedURL,
				"127.0.0.1",
				200,
				server.Port(),
				"1.1",
				"http",
			)
		})
	}

	t.Run("propagators_env", func(t *testing.T) {
		// 真正观察服务端收到的传播头，证明 OTEL_PROPAGATORS 影响出站请求，而非仅有本地 Span。
		f := testutil.NewTestFixture(t)
		f.SetEnv("OTEL_PROPAGATORS", "b3")

		var (
			mu      sync.Mutex
			headers http.Header
		)
		server := StartHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			headers = r.Header.Clone()
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"message":"Hello"}`)
		}))

		f.Run("httpclient", "-addr="+server.URL, "-name=world")

		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, headers.Get("b3"),
			"OTEL_PROPAGATORS=b3 should make the instrumented client inject the b3 header")
		require.Empty(t, headers.Get("traceparent"),
			"the default tracecontext propagator should be replaced, not composed")

		f.RequireSingleSpan()
	})

	t.Run("instrumentation_controls_env", func(t *testing.T) {
		// 同一个已编译二进制以不同环境运行，证明这是运行时组件开关，禁用列表覆盖允许列表。
		// f.SetEnv 仅修改 fixture 的子进程环境，适用于外层 Parallel；不能替换为全局 t.Setenv。
		tests := []struct {
			name         string
			enabled      string
			disabled     string
			expectedSpan bool
		}{
			{
				name:         "allow listed",
				enabled:      "nethttp",
				expectedSpan: true,
			},
			{
				name:    "not allow listed",
				enabled: "grpc",
			},
			{
				name:     "deny listed",
				disabled: "nethttp",
			},
			{
				name:     "deny list overrides allow list",
				enabled:  "nethttp",
				disabled: "nethttp",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				f := testutil.NewTestFixture(t)
				f.SetEnv("OTEL_GO_ENABLED_INSTRUMENTATIONS", tc.enabled)
				f.SetEnv("OTEL_GO_DISABLED_INSTRUMENTATIONS", tc.disabled)
				server := StartHTTPServerWithResponse(t, http.StatusOK, `{"message":"Hello"}`)

				f.Run("httpclient", "-addr="+server.URL, "-name=world")

				if tc.expectedSpan {
					f.RequireSingleSpan()
				} else {
					f.RequireTraceCount(0)
				}
			})
		}
	})
}

// HTTPServer wraps a test HTTP server.
type HTTPServer struct {
	t *testing.T
	*httptest.Server
}

func (s *HTTPServer) Port() int64 {
	u, err := url.Parse(s.URL)
	require.NoError(s.t, err)
	port, err := strconv.ParseInt(u.Port(), 10, 64)
	require.NoError(s.t, err)
	return port
}

// StartHTTPServer creates and starts a test HTTP server with a custom handler.
// The server is automatically closed when the test completes.
func StartHTTPServer(t *testing.T, handler http.Handler) *HTTPServer {
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &HTTPServer{t: t, Server: server}
}

// StartHTTPServerWithResponse creates a test HTTP server that returns the given status and body.
func StartHTTPServerWithResponse(t *testing.T, status int, body string) *HTTPServer {
	return StartHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprintln(w, body)
	}))
}
