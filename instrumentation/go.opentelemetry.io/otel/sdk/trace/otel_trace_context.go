//go:build ignore

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace

import (
	"container/list"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	_ "unsafe"

	trace "go.opentelemetry.io/otel/trace"
)

// 中文导读：本文件由 add_file 注入 go.opentelemetry.io/otel/sdk/trace。
// 它把“goroutine 上的一个 interface{} 字段”解释为 traceContext，管理当前 Span。
// 建议按 traceContextAddSpan -> add -> tail -> Clone -> traceContextDelSpan 阅读。
// tail 跳过带有 GLS ended 标记的节点，返回最近仍可作隐式父节点的 Span。
// ended 也可能由容量淘汰设置，不一定意味着 SDK Span 已执行 End。
// GLS 用于隐式查找，不替代网络请求头传播。

//go:linkname registerTraceAndSpanIDFunc go.opentelemetry.io/otelc/pkg/runtime.RegisterTraceAndSpanIDFunc
func registerTraceAndSpanIDFunc(f func() (string, string))

//go:linkname registerSpanFromGLSFunc go.opentelemetry.io/otelc/pkg/runtime.RegisterSpanFromGLSFunc
func registerSpanFromGLSFunc(f func() trace.Span)

// otelcLogger is reached by linkname, like the register funcs above, rather than
// by importing pkg/runtime: that package imports go.opentelemetry.io/otel/sdk/trace,
// so an import edge from this injected file closes a cycle and the build fails at
// link time with a pkg/runtime fingerprint mismatch.
//
// Call it at each use site rather than caching it in a package var. Without an
// import edge Go does not order pkg/runtime's init() before this package's, so a
// var initializer would run first, latch the slog.Default() fallback, and silently
// discard OTEL_LOG_LEVEL=debug output.
//
//go:linkname otelcLogger go.opentelemetry.io/otelc/pkg/runtime.Logger
func otelcLogger() *slog.Logger

const defaultGLSMaxSpans = 1000

// defaultMaxSpanStates bounds lifecycle bookkeeping. Evicted states are marked
// ended, so reaching the limit drops implicit propagation for the evicted span
// instead of retaining a stale parent for it.
const defaultMaxSpanStates = 100_000

var (
	otelGLSMaxSpans = defaultGLSMaxSpans
	maxSpanStates   = defaultMaxSpanStates
)

func init() {
	// 初始化时读取容量限制，并将实际 GLS 查询函数注册到公共 runtime 包。
	// 此处的 linkname 桥接避免 SDK trace -> pkg/runtime -> SDK trace 的导入环。
	if parsed, ok := positiveIntEnv("OTEL_GLS_MAX_SPANS"); ok {
		otelGLSMaxSpans = parsed
	}
	if parsed, ok := positiveIntEnv("OTEL_GLS_MAX_SPAN_STATES"); ok {
		maxSpanStates = parsed
	}
	registerTraceAndSpanIDFunc(GetTraceAndSpanID)
	registerSpanFromGLSFunc(spanFromGLS)
}

func positiveIntEnv(name string) (int, bool) {
	v := os.Getenv(name)
	if v == "" {
		return 0, false
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// 每个 goroutine 拥有自己的链表结构。n 是当前记录的节点数；sw 是栈顶；
// lcs 缓存本地根 Span。结束标记可共享，但链表的增删不跨 goroutine 共享。
type traceContext struct {
	sw  *spanWrapper
	n   int
	lcs trace.Span
}

// prev 指向此前的 Span。ended 是指针：Clone 后父子 goroutine 可以看见
// 同一个 Span 的结束状态，而各自拥有独立的 spanWrapper 节点。
type spanWrapper struct {
	span  trace.Span
	prev  *spanWrapper
	ended *atomic.Bool
}

type spanKey struct {
	traceID trace.TraceID
	spanID  trace.SpanID
}

// spanStateEntry pairs a span's ended flag with its position in the
// insertion-order eviction list.
type spanStateEntry struct {
	state *atomic.Bool
	elem  *list.Element
}

// spanStates bounds shared lifecycle bookkeeping across goroutines. order
// tracks insertion order (oldest at the front) so that once the map hits
// maxSpanStates, eviction always drops the oldest entry rather than an
// arbitrary one — Go map iteration order is randomized, so iterating the map
// itself to pick a victim can evict a span that is still genuinely in flight
// on an unrelated goroutine.
var spanStates = struct {
	// 嵌入 Mutex 使外层匿名结构体可以直接调用 Lock/Unlock。
	// 这个全局表跨 goroutine 共享，和上面“每个 goroutine 独享链表”的约束不同。
	sync.Mutex
	states map[spanKey]*spanStateEntry
	order  *list.List
}{
	states: make(map[spanKey]*spanStateEntry),
	order:  list.New(),
}

// evictOldestLocked drops the oldest tracked span state, marking it ended so
// any goroutine still holding it stops treating it as a live parent. Callers
// must hold spanStates.Mutex.
func evictOldestLocked() {
	// 容量淘汰只是停止把该 Span 当作隐式父节点，不调用 Span.End，
	// 也不取消业务请求。保守地标记 ended，避免无限保留生命周期状态。
	front := spanStates.order.Front()
	if front == nil {
		return
	}
	key, _ := front.Value.(spanKey)
	if entry, ok := spanStates.states[key]; ok {
		entry.state.Store(true)
		delete(spanStates.states, key)
	}
	spanStates.order.Remove(front)
	otelcLogger().Debug("GLS span state tracker at capacity, evicting oldest entry",
		"limit", maxSpanStates)
}

func stateForSpan(span trace.Span) *atomic.Bool {
	sc := span.SpanContext()
	if !sc.IsValid() {
		return &atomic.Bool{}
	}
	key := spanKey{sc.TraceID(), sc.SpanID()}
	// 用 TraceID + SpanID 作为可比较的 map 键，让同一 Span 的不同持有者
	// 找到同一个原子标记。先加锁再 defer 解锁，覆盖后续所有返回分支。
	spanStates.Lock()
	defer spanStates.Unlock()
	if entry, ok := spanStates.states[key]; ok {
		return entry.state
	}
	if len(spanStates.states) >= maxSpanStates {
		evictOldestLocked()
	}
	state := &atomic.Bool{}
	elem := spanStates.order.PushBack(key)
	spanStates.states[key] = &spanStateEntry{state: state, elem: elem}
	return state
}

func markSpanEnded(span trace.Span) {
	// 从全局 map 删除后，其他 goroutine 的节点仍持有 state 指针，
	// 因而仍能读到 true；Go 的 GC 不会释放仍被引用的对象。
	sc := span.SpanContext()
	if !sc.IsValid() {
		return
	}
	key := spanKey{sc.TraceID(), sc.SpanID()}
	spanStates.Lock()
	defer spanStates.Unlock()
	if entry, ok := spanStates.states[key]; ok {
		entry.state.Store(true)
		delete(spanStates.states, key)
		spanStates.order.Remove(entry.elem)
	}
}

func (tc *traceContext) compact() {
	// addr 的类型是 **spanWrapper：它指向“保存节点指针的位置”。
	// 令 *addr = 当前节点.prev 就能绕过当前节点，同时统一处理头节点和中间节点。
	// 这比只修改 cur 局部变量多了一层间接寻址，才能真正改动链表连接。
	addr := &tc.sw
	for *addr != nil {
		if (*addr).ended.Load() {
			*addr = (*addr).prev
			tc.n--
			continue
		}
		addr = &(*addr).prev
	}
	tc.lcs = nil
	for cur := tc.sw; cur != nil; cur = cur.prev {
		tc.lcs = cur.span
	}
}

func (tc *traceContext) add(span trace.Span) bool {
	// 达到上限先清理已结束节点，仍满则不加入 GLS。
	// 返回 false 仅表示不做隐式追踪；Span 对象本身已由 SDK 创建，仍可显式使用。
	if tc.n >= otelGLSMaxSpans {
		tc.compact()
		if tc.n >= otelGLSMaxSpans {
			// The new span is dropped from GLS: it won't be reachable via
			// implicit propagation, and any span.SpanFromContext(context.Background())
			// lookup on this goroutine keeps returning whatever was already
			// on top of the stack until it compacts below the limit. Surface
			// that instead of dropping it silently.
			otelcLogger().Debug("GLS span stack at capacity, span not tracked for implicit propagation",
				"limit", otelGLSMaxSpans)
			return false
		}
	}
	wrapper := &spanWrapper{span, tc.sw, stateForSpan(span)}
	if tc.n == 0 {
		tc.lcs = span
	}
	tc.sw = wrapper
	tc.n++
	return true
}

// tail must be called only on the current goroutine's own context: it mutates the
// stack, whose list fields are unsynchronized. Only ended flags are shared.
//
// 中文说明：读取时顺便弹出已结束的栈顶，恢复此前还活跃的 Span。
// norace 是现有实现的编译指令，不会提供同步能力；安全性仍依赖链表不共享。
//
//go:norace
func (tc *traceContext) tail() trace.Span {
	for tc.sw != nil && tc.sw.ended.Load() {
		tc.sw = tc.sw.prev
		tc.n--
	}
	if tc.sw == nil {
		tc.lcs = nil
		return nil
	}
	return tc.sw.span
}

func (tc *traceContext) localRootSpan() trace.Span {
	if tc.n == 0 {
		return nil
	} else {
		return tc.lcs
	}
}

func (tc *traceContext) del(span trace.Span) {
	// Span 不一定按创建的逆序结束，因此不能简单地“每次 End 弹出栈顶”。
	// 遍历 TraceID/SpanID 找到实际节点，删除后修正节点数及本地根缓存。
	if tc.n == 0 {
		return
	}
	addr := &tc.sw
	cur := tc.sw
	for cur != nil {
		sc1 := cur.span.SpanContext()
		sc2 := span.SpanContext()
		if sc1.TraceID() == sc2.TraceID() && sc1.SpanID() == sc2.SpanID() {
			cur.ended.Store(true)
			*addr = cur.prev
			tc.n--
			if cur.prev == nil {
				tc.lcs = nil
				for remaining := tc.sw; remaining != nil; remaining = remaining.prev {
					tc.lcs = remaining.span
				}
			}
			break
		}
		addr = &cur.prev
		cur = cur.prev
	}
}

func (tc *traceContext) clear() {
	tc.sw = nil
	tc.n = 0
	tc.lcs = nil
	runtime.SetBaggageContainerToGLS(nil)
}

// Clone 在创建子 goroutine 时调用：只继承当前活跃 Span，不复制父的完整栈。
// 新链表只有一个节点，ended 指针仍共享；子 goroutine 再 Start/End 自己的 Span
// 不会改坏父的链表。克隆初始状态的 lcs 为 nil；compact 等操作会重新计算该缓存。
//
//go:norace
func (tc *traceContext) Clone() interface{} {
	last := tc.tail()
	if last == nil {
		return &traceContext{nil, 0, nil}
	}
	sw := &spanWrapper{last, nil, tc.sw.ended}
	return &traceContext{sw, 1, nil}
}

func GetTraceContext() trace.SpanContext {
	t := getOrInitTraceContext()
	if span := t.tail(); span != nil {
		return span.SpanContext()
	}
	return trace.SpanContext{}
}

func getOrInitTraceContext() *traceContext {
	// 惰性初始化：当前 goroutine 首次需要上下文时才分配。
	// tc.(*traceContext) 是单返回值类型断言，依赖该 runtime 字段只保存约定类型。
	tc := runtime.GetTraceContextFromGLS()
	if tc == nil {
		newTc := &traceContext{nil, 0, nil}
		setTraceContext(newTc)
		return newTc
	} else {
		return tc.(*traceContext)
	}
}

func setTraceContext(tc *traceContext) {
	runtime.SetTraceContextToGLS(tc)
}

func traceContextAddSpan(span trace.Span) {
	tc := getOrInitTraceContext()
	if tc.add(span) {
		setTraceContext(tc)
	}
}

func GetTraceAndSpanID() (string, string) {
	tc := runtime.GetTraceContextFromGLS()
	if tc == nil {
		return "", ""
	}
	span := tc.(*traceContext).tail()
	if span == nil {
		return "", ""
	}
	ctx := span.SpanContext()
	return ctx.TraceID().String(), ctx.SpanID().String()
}

func traceContextDelSpan(span trace.Span) {
	// 先通知共享状态，再删除当前 goroutine 的节点。即使 Span 在另一个
	// goroutine 结束，继承者下一次 tail 查找也能跳过它，避免把过期 Span 当父节点。
	markSpanEnded(span)
	if ctx := runtime.GetTraceContextFromGLS(); ctx != nil {
		ctx.(*traceContext).del(span)
	}
}

func ClearTraceContext() {
	getOrInitTraceContext().clear()
}

func spanFromGLS() trace.Span {
	gls := runtime.GetTraceContextFromGLS()
	if gls == nil {
		return nil
	}
	return gls.(*traceContext).tail()
}
