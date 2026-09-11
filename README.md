# StompWS - STOMP WebSocket 服务器

基于 STOMP 协议和 WebSocket 实现的高性能聊天室服务器，完整支持 STOMP 1.0/1.1/1.2 协议规范。

## 📚 项目简介

基于 WebSocket + STOMP 的实时聊天室系统，提供基于 STOMP 协议的实时通信能力，支持群聊、私聊、消息队列等功能。本实现参考了
`github.com/go-stomp/stomp/v3/server/client` 包，并实现了其核心功能，完成度达到 **100%**。

## ✨ 核心特性

### STOMP 协议支持

**所有标准命令**

- ✅ CONNECT/STOMP - 连接握手与版本协商
- ✅ SUBSCRIBE/UNSUBSCRIBE - 订阅管理
- ✅ SEND/MESSAGE - 消息发送与接收
- ✅ ACK/NACK - 消息确认（支持 auto/client/client-individual）
- ✅ BEGIN/COMMIT/ABORT - 事务支持
- ✅ RECEIPT - 操作确认
- ✅ ERROR - 错误响应
- ✅ DISCONNECT - 优雅断开

**协议特性**

- ✅ 版本协商（1.0, 1.1, 1.2）
- ✅ 心跳机制（读写超时检测）
- ✅ 帧验证器（必需头部检查）
- ✅ 订阅冲突检测
- ✅ 连接状态管理

**消息模式**

- ✅ Topic（广播）- `/topic/*` - 发布订阅模式，一对多消息广播
- ✅ Queue（点对点）- `/queue/*` - 消息队列模式，负载均衡分发
- ✅ 私聊 - `/user/{username}` - 一对一私密通信
- ✅ 消息重入队（未确认消息）

### 聊天室功能

- 用户上线/下线通知
- 在线用户列表
- 群组消息广播
- 一对一私聊
- 特殊命令支持（/help、/online、/time、/support）

## 🏗️ 项目结构

```
stompws/
├── main.go                      # 服务入口
├── go.mod                       # Go 模块文件
├── go.sum                       # 依赖校验文件
├── logws/                       # 日志系统
│   └── logger.go               # Logger 接口与实现
├── server/                      # 服务端实现
│   ├── client/                 # 客户端连接管理
│   │   ├── server.go          # STOMP 服务器主逻辑
│   │   ├── client.go          # 客户端连接管理（三协程模型）
│   │   ├── handlers.go        # STOMP 命令处理
│   │   ├── auth.go            # 身份验证
│   │   ├── errors.go          # 错误定义
│   │   ├── hook.go            # 事件钩子
│   │   ├── subscription.go    # 订阅管理
│   │   ├── subscription_list.go # 订阅列表
│   │   ├── tx_store.go        # 事务存储
│   │   ├── validator.go       # 帧验证器
│   │   └── util.go            # 工具函数
│   ├── topic/                 # Topic 管理器（广播）
│   │   ├── manager.go         # Topic 管理
│   │   ├── topic.go           # Topic 实现
│   │   └── subscription.go    # Topic 订阅
│   └── queue/                 # Queue 管理器（点对点）
│       ├── manager.go         # Queue 管理
│       ├── queue.go           # Queue 实现
│       ├── memory_queue.go    # 内存队列实现
│       ├── storage.go         # 消息存储接口
│       ├── subscription.go    # Queue 订阅
│       └── subscription_list.go # 订阅列表
└── web/                        # Web 客户端
    └── client.html            # 测试页面
```

### 核心组件

**StompHubServer**

- 客户端管理（sync.Map）
- Topic 管理器（广播）
- Queue 管理器（点对点）
- 在线用户列表

**Client**

- 三协程模型：readLoop / writeLoop / processLoop
- 心跳检测（读写超时）
- 订阅管理
- 事务存储
- 帧验证器

**TxStore**

- 事务帧缓存
- BEGIN/COMMIT/ABORT 支持
- 自动清理

## 🚀 快速开始

### 环境要求

- Go 1.23+
- 支持 WebSocket 的浏览器

### 启动服务

```bash
# 进入项目目录
cd stompws

# 安装依赖
go mod tidy

# 启动服务
go run main.go
```

服务将在 `http://localhost:9091` 启动，WebSocket 端点为 `ws://localhost:9091/admin-api/v1/websocket`。

### 访问测试页面

启动服务后，直接访问 `http://localhost:9091` 即可打开测试页面。

### 基本操作

1. 输入用户名（默认无需密码，可配置身份验证）
2. 连接服务器（自动协商 STOMP 版本 1.0/1.1/1.2）
3. 订阅主题（如 `/topic/chat`）
4. 发送消息

### 测试账号

如果启用了密码认证，可使用以下测试账号：
- guest/guest
- admin/admin

### 消息类型

- **广播消息**：发送到 `/topic/chat` 等主题，所有订阅者都能收到
- **私信消息**：发送到 `/user/用户名`，只有指定用户能收到
- **队列消息**：发送到 `/queue/名称`，负载均衡分发给订阅者

## 🔧 配置选项

### 身份验证

支持两种认证模式：

**无需认证（默认）**
```go
server := client.NewStompHubServer(
    client.WithAuthenticator(client.NewNoAuthenticator()),
)
```

**密码认证**
```go
auth := client.NewPasswordAuthenticator()
auth.AddUser("alice", "password123")
auth.AddUser("bob", "secret456")

server := client.NewStompHubServer(
    client.WithAuthenticator(auth),
)
```

### 事件钩子

支持自定义事件钩子，可在连接、断开、消息发送等时机执行自定义逻辑。

### 自定义日志

支持自定义日志实现，只需实现 Logger 接口即可。

## 🎯 技术亮点

### 1. 完整的 STOMP 协议实现

- 严格遵循 STOMP 1.0/1.1/1.2 规范
- 帧验证（必需头部、格式检查）
- 心跳格式验证（正则 + 最大值限制）
- 订阅冲突检测
- 事务原子性保证

### 2. 高并发设计

- 三协程模型（读/写/处理分离）
- 非阻塞消息发送
- Channel 通信避免锁竞争
- sync.Map 并发安全

### 3. 可靠性保证

- 心跳超时检测
- 未确认消息重入队
- 优雅断开连接
- 事务回滚支持
- 错误时包含 receipt-id
- 身份验证（防暴力破解）

### 4. 代码质量

- 使用 stomp 包常量（版本、命令、头部）
- 标准化错误消息（英文常量）
- 完整的日志系统（Logger 接口）
- 可插拔身份验证（Authenticator 接口）
- 清晰的职责分离
- 完善的错误处理
- 详细的代码注释

## 📊 与 server/client 包对比

本实现参考了 `github.com/go-stomp/stomp/v3/server/client` 包，并实现了其核心功能：

| 功能      | server/client | 本实现 | 说明               |
|---------|---------------|-----|------------------|
| 基本命令    | ✅             | ✅   | 完全兼容             |
| 版本协商    | ✅             | ✅   | 1.0/1.1/1.2      |
| 心跳机制    | ✅             | ✅   | 读写超时             |
| 帧验证     | ✅             | ✅   | 自定义实现            |
| 事务支持    | ✅             | ✅   | 完全兼容             |
| RECEIPT | ✅             | ✅   | 完全支持             |
| 订阅冲突检测  | ✅             | ✅   | 完全支持             |
| 身份验证    | ✅             | ✅   | Authenticator 接口 |
| 错误消息标准化 | ✅             | ✅   | 英文错误常量           |
| 日志系统    | ✅             | ✅   | Logger 接口        |

**完成度：100%**

## ⚡ 性能优化

### 已优化

- ✅ 三协程模型（读写分离）
- ✅ 非阻塞发送（超时机制）
- ✅ Channel 通信（减少锁）
- ✅ 心跳检测（自动清理死连接）

### 可优化

- 对象池复用（sync.Pool）
- 消息批量发送
- 连接数限制
- 消息持久化（数据库）
- 集群支持（Redis 发布订阅）

## 🧪 测试

### 运行单元测试

```bash
# 运行所有测试
go test ./...

# 运行特定模块测试
go test ./server/topic/
go test ./server/queue/

# 运行测试并显示覆盖率
go test -cover ./...
```

### 功能测试建议

**基本连接测试**
- 测试 CONNECT/DISCONNECT 命令
- 测试版本协商（1.0, 1.1, 1.2）
- 测试心跳超时机制

**订阅测试**
- 测试 SUBSCRIBE/UNSUBSCRIBE 命令
- 测试订阅冲突检测
- 测试不同 ACK 模式（auto/client/client-individual）

**事务测试**
- 测试 BEGIN/COMMIT 事务流程
- 测试 ABORT 回滚机制
- 测试事务冲突处理

**消息测试**
- 测试 Topic 广播模式
- 测试 Queue 点对点模式
- 测试消息重入队机制

### 压力测试建议

```bash
# 并发连接测试
- 1000+ 并发客户端连接
- 持续消息发送测试
- 监控内存和 CPU 使用率

# 消息吞吐测试
- 每秒 10000+ 条消息
- 测试消息延迟和丢包率
- 测试长时间运行稳定性
```

## 📄 开源协议

Apache License 2.0

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

### 可选增强功能

- 消息持久化
- 集群支持
- 监控指标
- TLS/WSS 支持

---

## 🙏 致谢

本项目参考了 [go-stomp/stomp](https://github.com/go-stomp/stomp) 的设计思想。

## 🔗 相关链接

- [STOMP 协议规范](https://stomp.github.io/)
- [go-stomp/stomp](https://github.com/go-stomp/stomp)

## 📝 代码示例

### 基本使用示例

```go
package main

import (
	"net/http"
	"github.com/ve-weiyi/stompws/server/client"
	"github.com/ve-weiyi/stompws/logws"
)

func main() {
	// 创建服务器
	server := client.NewStompHubServer(
		client.WithEventHooks(client.NewDefaultEventHook()),
		client.WithAuthenticator(client.NewNoAuthenticator()),
		client.WithLogger(logws.NewDefaultLogger()),
	)

	// 注册 WebSocket 路由
	http.HandleFunc("/admin-api/v1/websocket", server.HandleWebSocket)
	
	// 静态文件服务
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "client.html")
	})

	// 启动服务
	http.ListenAndServe(":9091", nil)
}
```

### 身份验证配置

```go
// 无需认证（默认）
server := client.NewStompHubServer(
    client.WithAuthenticator(client.NewNoAuthenticator()),
)

// 密码认证
auth := client.NewPasswordAuthenticator()
auth.AddUser("alice", "password123")
auth.AddUser("bob", "secret456")

server := client.NewStompHubServer(
    client.WithAuthenticator(auth),
)
```

### 自定义事件钩子

```go
type MyEventHook struct {
client.DefaultEventHook
}

func (h *MyEventHook) OnConnect(server *client.StompHubServer, c *client.Client) {
// 用户连接时的自定义逻辑
}

func (h *MyEventHook) OnSend(server *client.StompHubServer, c *client.Client, message *frame.Frame) bool {
// 消息发送前的拦截处理
return true // 返回 false 可阻止消息发送
}

server := client.NewStompHubServer(
client.WithEventHooks(&MyEventHook{}),
)
```

### 自定义日志实现

```go
type MyLogger struct{}

func (l *MyLogger) Infof(format string, args ...interface{}) {
// 自定义日志实现
}

server := client.NewStompHubServer(
client.WithLogger(&MyLogger{}),
)
```

### JavaScript 客户端示例

```javascript
// 连接服务器
const socket = new WebSocket('ws://localhost:9091/admin-api/v1/websocket');
const client = Stomp.over(socket);

// 连接并订阅
client.connect({}, function (frame) {
    console.log('Connected:', frame);

    // 订阅 Topic
    client.subscribe('/topic/chat', function (message) {
        console.log('Received:', message.body);
    });

    // 发送消息
    client.send('/topic/chat', {}, JSON.stringify({
        username: 'Alice',
        content: 'Hello World!'
    }));
});
```

### 事务使用示例

```javascript
// 开始事务
const tx = 'tx-' + Date.now();
client.begin(tx);

// 批量发送（原子操作）
client.send('/queue/orders', {transaction: tx}, order1);
client.send('/queue/orders', {transaction: tx}, order2);
client.send('/queue/notifications', {transaction: tx}, notification);

// 提交事务（原子执行）
client.commit(tx);

// 或回滚
// client.abort(tx);
```
