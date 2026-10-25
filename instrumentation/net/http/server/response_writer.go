// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Compile-time assertions that writerWrapper satisfies the optional interfaces
// an http.ResponseWriter may implement.
var (
	_ http.ResponseWriter = (*writerWrapper)(nil)
	_ http.Hijacker       = (*writerWrapper)(nil)
	_ http.Flusher        = (*writerWrapper)(nil)
	_ http.Pusher         = (*writerWrapper)(nil)
	_ io.ReaderFrom       = (*writerWrapper)(nil)
	_ io.StringWriter     = (*writerWrapper)(nil)
)

// writerWrapper wraps http.ResponseWriter to capture the status code
// 中文导读：嵌入 ResponseWriter 让 Header 等方法继续转发，额外重写的方法负责保持语义。
// 包装接口时必须检查可选能力，否则业务的流式刷新、连接劫持和零拷贝路径可能受到影响。
// 当前包装器始终实现下面的可选接口，具体调用时才检查底层能力，不代表底层一定支持它们。
type writerWrapper struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

// WriteHeader captures the final status code and forwards to the underlying ResponseWriter.
func (w *writerWrapper) WriteHeader(statusCode int) {
	// 最终响应只能提交一次；后续 WriteHeader 不应覆盖实际已发送的状态。
	if w.wroteHeader {
		return
	}

	// Informational responses do not commit the final response. Switching
	// protocols is the exception: net/http treats 101 as final.
	// 例如 103 Early Hints 之后仍可发送最终 200；101 Switching Protocols 则视为最终响应。
	if statusCode >= 100 && statusCode <= 199 && statusCode != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(statusCode)
		return
	}

	w.statusCode = statusCode
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

// Write implements http.ResponseWriter.Write and ensures WriteHeader is called
func (w *writerWrapper) Write(b []byte) (int, error) {
	// If WriteHeader wasn't called yet, call it with 200 OK (default HTTP behavior)
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// writeOnly hides every method of the wrapped value except Write, so that
// io.Copy cannot re-enter a ReadFrom fast path through it.
type writeOnly struct {
	io.Writer
}

// ReadFrom implements io.ReaderFrom so that io.Copy keeps reaching the
// underlying ResponseWriter's own ReadFrom. net/http's *response implements
// io.ReaderFrom and hands the body to the connection, which lets the kernel
// serve it with sendfile(2). Without this method the wrapper hides that
// capability from io.Copy, and http.ServeFile/http.ServeContent silently fall
// back to a 32KiB user-space copy loop on every instrumented server.
func (w *writerWrapper) ReadFrom(src io.Reader) (int64, error) {
	// 新组件若包装 io.Writer，不能只保证功能正确，还应保留 io.Copy 的可选快路径。
	// http.ResponseWriter.Write implies a 200 when no status was set; ReadFrom
	// writes the body the same way, so record it before handing the body off.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}

	// The underlying writer has no fast path, so fall back to a plain copy
	// through Write. writeOnly keeps io.Copy from picking this method up again.
	return io.Copy(writeOnly{w}, src)
}

// WriteString implements io.StringWriter so that io.WriteString keeps
// reaching the underlying ResponseWriter's own WriteString. net/http's
// *response implements io.StringWriter to write the string without an extra
// []byte conversion. Without this method io.WriteString falls back to
// converting to []byte and calling Write, which is the same fast-path loss
// ReadFrom fixes above, just for string writes instead of io.Copy.
func (w *writerWrapper) WriteString(s string) (int, error) {
	// http.ResponseWriter.Write implies a 200 when no status was set;
	// WriteString writes the body the same way, so record it first.
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if sw, ok := w.ResponseWriter.(io.StringWriter); ok {
		return sw.WriteString(s)
	}

	// The underlying writer has no fast path, so fall back to a plain Write.
	return w.ResponseWriter.Write([]byte(s))
}

// Hijack implements the http.Hijacker interface
func (w *writerWrapper) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	// WebSocket 等升级场景需要把连接控制权交给业务，不能因插桩而隐藏这个能力。
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("responseWriter does not implement http.Hijacker")
}

// Flush implements the http.Flusher interface
func (w *writerWrapper) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Push implements the http.Pusher interface, forwarding to the underlying
// ResponseWriter when it supports HTTP/2 server push and returning
// http.ErrNotSupported otherwise.
func (w *writerWrapper) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *writerWrapper) Unwrap() http.ResponseWriter {
	// ResponseController 可以沿 Unwrap 继续访问底层连接控制能力。
	return w.ResponseWriter
}
