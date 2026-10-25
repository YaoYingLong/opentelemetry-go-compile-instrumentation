//go:build ignore

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package runtime

// 本文件是 add_file 规则的源材料。顶部 build ignore 让普通模块构建忽略它，
// otelc 则在编译标准库 runtime 时把它注入目标包；所以这里能访问未导出的
// getg 和 g 字段。不要把它当成能被应用直接 import 的普通辅助包。
// 与 Java 的线程本地存储类比时，要把“线程”换成“goroutine”：
// Go 调度器可让同一个 goroutine 在不同 OS 线程上运行。

/*
	编译时给 Go runtime 的 goroutine 结构增加字段，并在创建新 goroutine 时，将父 goroutine 的当前 Span 继承过去
	数据附着在 goroutine 上，因此 goroutine 被调度到不同线程时，仍然访问自己的那份数据。
*/

func GetTraceContextFromGLS() interface{} {
	// getg().m.curg 指向当前 m（OS 线程对应的 runtime 对象）上的用户 goroutine。
	// 字段使用 interface{}，使底层 runtime 不需要依赖 OTel SDK 的具体类型。
	// 从当前 goroutine 的上下文通过以下方式读取
	return getg().m.curg.otel_trace_context
}

func GetBaggageContainerFromGLS() interface{} {
	return getg().m.curg.otel_baggage_container
}

func SetTraceContextToGLS(traceContext interface{}) {
	// 通过以下方式将traceContext写入当前 goroutine 的上下文
	getg().m.curg.otel_trace_context = traceContext
}

func SetBaggageContainerToGLS(baggageContainer interface{}) {
	getg().m.curg.otel_baggage_container = baggageContainer
}

// SuppressHooks turns off Before and After hooks on the current goroutine,
// for calls made while the count stays above zero.
func SuppressHooks() {
	// 使用计数器支持嵌套抑制；调用者必须用对应的 UnsuppressHooks 成对恢复。
	// 这里只抑制当前 goroutine 上的 hook，不停止业务函数，也不关闭整个 SDK。
	getg().m.curg.otel_ignored_call_count++
}

// UnsuppressHooks reverses one call to SuppressHooks.
func UnsuppressHooks() {
	getg().m.curg.otel_ignored_call_count--
}

// HooksSuppressed reports whether the current goroutine is inside a call
// bracketed by SuppressHooks.
//
// A goroutine started while the count is above zero starts at zero, so a
// hook running on that new goroutine is not suppressed.
func HooksSuppressed() bool {
	return getg().m.curg.otel_ignored_call_count > 0
}

type OtelContextCloner interface {
	// Go 接口是隐式实现：只要具体类型拥有这个签名的 Clone 方法就满足接口，
	// 不需要像 Java 那样在类型声明上写 implements。
	Clone() interface{}
}

func propagateOtelContext(context interface{}) interface{} {
	// 这里名为 context 的参数只是一个 interface{} 变量，不是 context 包。
	// newproc1 的注入代码在新 goroutine 创建时调用它；不会在每次调度时重拷贝。
	if context == nil {
		return nil
	}
	if cloner, ok := context.(OtelContextCloner); ok {
		// 若对象提供复制协议就按协议继承；traceContext 只复制当前 Span 的包装，
		// 不共享父 goroutine 的可变链表。没有协议的对象则原样传递，可能仍共享引用。
		return cloner.Clone()
	}
	return context
}
