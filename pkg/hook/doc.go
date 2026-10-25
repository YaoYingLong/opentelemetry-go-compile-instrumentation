// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package hook 定义插桩函数与目标函数之间的通信接口。
//
// 中文阅读入口：先读本文件，再读 context.go 的 HookContext，最后对照
// instrumentation/net/http/client/client_hook.go 的 BeforeRoundTrip/AfterRoundTrip。
// HookContext 的具体实现由 tool/internal/instrument 为目标函数生成，
// 因而这里没有一个适用于所有目标函数的手写实现。
//
// # 两种“上下文”
//
// HookContext 描述“一次被拦截的函数调用”：参数、返回值、是否跳过原函数、
// Before/After 之间共享的数据。context.Context 描述请求的取消、截止时间和值，
// OTel 将当前 Span 放在后者中。HookContext 自身不是 context.Context；
// 可以通过 SetData 把后者保存在前者中，供同一次调用的 After 使用。
//
// # 参数、接收者和返回值
//
// 对 func (t *Transport) RoundTrip(req *Request) (*Response, error)，
// 参数槽位 0 是接收者 t，槽位 1 才是 req；返回值槽位 0 是响应，1 是 error。
// 普通函数没有接收者，第一个显式参数就是槽位 0。Go 的方法接收者可以是值
// 或指针，具体签名与 YAML 中的 recv 必须对应。
// Before 的第一个参数固定是 HookContext，随后对应目标的接收者和入参；
// After 的第一个参数也是 HookContext，随后对应目标返回值。
//
// Go 参数按值传递：在 Before 内写 req = newReq 只改变 hook 的局部变量。
// 要替换被拦截调用中的参数，应使用 SetParam；替换返回值使用 SetReturnVal。
// 生成器按目标类型生成访问代码，普通具体类型通过保存的地址读写；
// any/interface{} 与泛型参数有专门分支，阅读 trampoline.go 的
// rewriteHookContextMethods 时再研究这些边界，不应假设其内部全是反射。
// any 是 interface{} 的别名；接口可以持有不同动态类型，不代表可随意赋错类型。
//
// # 一次调用中的状态与短路
//
// SetData/GetData 适合传递一个自定义状态结构体；SetKeyData/GetKeyData
// 适合传递多项命名数据。两组方法使用同一个底层 data 字段，不能当成两块
// 独立存储混用，否则后写的数据会覆盖前一种表示。
//
// SetSkipCall(true) 请求跳过原函数体，但后续仍会调用 After。
// 若 Before 已获得替代结果，可通过 SetData 保存并请求跳过，由 After 使用
// SetReturnVal 写回；Before 阶段尚未填充返回值槽位，不应在此直接写返回值。
// 目标函数入口的生成代码负责安排 After/defer，trampoline 为 hook 提供局部
// panic 保护。业务函数自身的 panic 不因此自动变成普通 error。
package hook
