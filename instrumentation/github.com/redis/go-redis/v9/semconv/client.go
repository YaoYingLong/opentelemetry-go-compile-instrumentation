// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package semconv

import (
	"net"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// 中文导读：把从库 API 提取到的数据转成 OTel 属性的中间对象，隔离 Hook 和语义约定。
// 新组件也可先定义请求描述，再集中生成属性，避免每个入口各自拼接属性名。
type RedisRequest struct {
	Endpoint  string
	FullName  string
	Statement string
}

// redisClientRequestAttrCap is the most attributes RedisClientRequestTraceAttrs
// can return: five required fields plus server.port.
const redisClientRequestAttrCap = 6

// RedisClientRequestTraceAttrs returns trace attributes for a Redis client request.
func RedisClientRequestTraceAttrs(req RedisRequest) []attribute.KeyValue {
	// 标准 host:port 拆成 server.address/server.port；若传入 Conn.String() 这类描述文本，
	// 拆分失败就完整保留为 server.address，不凭空猜测端口。集成测试对此区别有专门断言。
	host, portStr, err := net.SplitHostPort(req.Endpoint)
	if err != nil {
		host = req.Endpoint
	}

	attrs := make([]attribute.KeyValue, 0, redisClientRequestAttrCap)
	// 这里使用导入的 semconv/v1.37.0 常量，避免旧 db.system/db.statement 等名称混入。
	// 当前核心字段为 db.system.name、db.operation.name、db.query.text 和网络端点。
	attrs = append(attrs,
		semconv.DBSystemNameRedis,
		semconv.DBOperationName(req.FullName),
		semconv.ServerAddress(host),
		semconv.NetworkTransportTCP,
		semconv.DBQueryText(req.Statement),
	)

	if err == nil {
		if port, convErr := strconv.Atoi(portStr); convErr == nil && port > 0 {
			attrs = append(attrs, semconv.ServerPort(port))
		}
	}

	return attrs
}
