// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package runtime

import "context"

// 使用本包私有的空结构体类型作为 key，避免与其他包使用的字符串 key 冲突。
// struct{} 不存业务字段；这里利用的是它的类型身份。
type contextKey struct{}

var suppressHTTPClientKey = contextKey{}

// SuppressHTTPClientInstrumentation returns a context that signals the net/http
// client hook to skip span creation. Use this from higher-level instrumentations
// (e.g., GenAI) that already create a more specific span.
func SuppressHTTPClientInstrumentation(ctx context.Context) context.Context {
	// WithValue 派生新的 context，原 ctx 不会被就地修改。
	// 调用方必须继续传递返回值，这个标记才会沿请求流到 HTTP client hook。
	// 它只抑制 HTTP client 插桩，和 runtime 的 goroutine 级全部 hook 抑制不同。
	return context.WithValue(ctx, suppressHTTPClientKey, true)
}

// IsHTTPClientInstrumentationSuppressed reports whether the context carries the
// suppression flag set by SuppressHTTPClientInstrumentation.
func IsHTTPClientInstrumentationSuppressed(ctx context.Context) bool {
	// 两返回值类型断言在不存在/类型不符时得到 false，不会因断言失败而 panic。
	v, _ := ctx.Value(suppressHTTPClientKey).(bool)
	return v
}
