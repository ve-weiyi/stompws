# StompWS - STOMP WebSocket 服务器

基于 STOMP 协议和 WebSocket 实现的高性能聊天室服务器，完整支持 STOMP 1.0/1.1/1.2 协议规范。

## 📚 项目简介

基于 WebSocket + STOMP 的实时聊天室系统，提供基于 STOMP 协议的实时通信能力，支持群聊、私聊、消息队列等功能。本实现参考了
`github.com/go-stomp/stomp/v3/server/client` 包，并实现了其核心功能：**命令与语义层已对齐上游 v3.2.0，中继状态的所有权也与上游
同构**（见下文「中继状态的所有权」）。

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
- ✅ 私聊 - `/queue/{username}` - 一对一私密通信（服务端只认 `/topic/` 与 `/queue/` 前缀，`/user/` 不存在）
- ✅ 消息重入队（未确认消息）

### 聊天室功能

这些**不在本服务内**，而由消费方的 `EventHook` 实现（本仓 `main.go` 用的是默认 hook，所以独立运行时不会生效）：

- 用户上线/下线通知 —— 由 hook 的 `OnConnect` / `OnDisconnect` 里自行广播
- 在线用户列表 —— 由 `PresenceManager` 提供数据（`server.Presence()` 查名单与计数），hook 负责推给客户端
- 群组消息广播、一对一私聊 —— 由 hook 在 `OnSend` 里调 `RouteMessage` 完成（默认 hook 不路由）
- 特殊命令（`/help`、`/online`、`/time`、`/support`）—— 本服务**零实现**，需要 hook 自己解析消息体

## 🏗️ 项目结构

```
stompws/
├── main.go                      # 服务入口
├── go.mod                       # Go 模块文件
├── go.sum                       # 依赖校验文件
├── logws/                       # 日志系统
│   └── logger.go               # Logger 接口与实现
├── server/                      # 服务端：hub + 中继 + 扩展点（package server）
│   ├── server.go              # StompHubServer：对外 API、可配置项、构造与关停
│   ├── hub.go                 # Hub 接口在服务端的实现（含钩子触发与会话登记）
│   ├── processor.go           # 中继循环：topic/queue 状态的唯一所有者
│   ├── request.go             # 中继请求的类型定义
│   ├── hook.go                # 事件钩子
│   ├── presence.go            # 在线状态的存储端口（谁在线）
│   ├── memory_presence.go     # 内置内存实现
│   ├── destination.go         # 目的地前缀与判定（唯一的归属判据）
│   ├── errors.go              # 服务端与中继的错误文案
│   ├── client/                # 一条连接（package client）
│   │   ├── conn.go            # Conn 类型、构造 NewConn、传输侧与 Serve
│   │   ├── config.go          # Config：服务端注入的连接级参数（日志、认证器、超时）
│   │   ├── hub.go             # 连接层与中继之间的接口（11 方法，含逐条协程契约）
│   │   ├── handlers.go        # 客户端来的帧：processLoop、调度与各命令实现、断开清理
│   │   ├── reply.go           # 回给客户端的帧：RECEIPT 与 ERROR
│   │   ├── subscription.go    # 订阅本身：类型、构造、给帧补 subscription 头、投递入口
│   │   ├── subscription_list.go # 待确认订阅表
│   │   ├── tx_store.go        # 事务存储
│   │   ├── validator.go       # 帧验证器
│   │   ├── frame.go           # 帧编解码与连接协商（版本、心跳）
│   │   ├── framereader.go    # 从 WebSocket 消息读帧，并守住帧的头段/正文上限
│   │   ├── errors.go          # 协议错误文案（唯一来源）
│   │   └── auth.go            # 身份验证（接口 + 默认实现）
│   ├── topic/                 # Topic 管理器（广播）
│   │   ├── manager.go         # Topic 管理
│   │   ├── topic.go           # Topic 实现
│   │   └── subscription.go    # Topic 订阅
│   └── queue/                 # Queue 管理器（点对点，与上游逐字一致）
│       ├── manager.go         # Queue 管理
│       ├── queue.go           # Queue 实现
│       ├── memory_queue.go    # 内存队列实现
│       └── storage.go         # 消息存储接口
└── web/                        # Web 客户端
    └── client.html            # 测试页面
```

### 核心组件

**StompHubServer**

- 客户端管理（sync.Map）
- Topic 管理器（广播）
- Queue 管理器（点对点）
- 在线用户列表

**Conn**

- 三协程模型：readLoop / writeLoop / processLoop
- 心跳检测（读写超时）
- 订阅管理
- 事务存储
- 帧验证器

**TxStore**

- 事务帧缓存
- BEGIN/COMMIT/ABORT 支持
- 连接断开时清空待提交帧（`cleanupClient`）—— 没有定时/容量维度的自动清理

## 🚀 快速开始

### 环境要求

- Go 1.24+（见 `go.mod`）
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
- **私信消息**：发送到 `/queue/user/用户名`（目的地必须以 `/topic/` 或 `/queue/` 开头），只有该队列的订阅者能收到
- **队列消息**：发送到 `/queue/名称`，负载均衡分发给订阅者

## 🔧 配置选项

### 身份验证

支持两种认证模式：

**无需认证（默认）**
```go
hub := server.NewStompHubServer(
    server.WithAuthenticator(server.NewNoAuthenticator()),
)
```

**自定义认证**
实现 `client.Authenticator` 交给服务端即可（下面以 token 校验为例；需 import
`"github.com/go-stomp/stomp/v3/frame"` 与 `"github.com/ve-weiyi/stompws/server/client"`）：

```go
type tokenAuthenticator struct {
    verifier *TokenVerifier
}

func (a *tokenAuthenticator) Authenticate(c *client.Conn, f *frame.Frame) (clientId, login string, err error) {
    login = f.Header.Get("login")
    clientId = f.Header.Get("client")
    if err := a.verifier.Validate(login, f.Header.Get("passcode")); err != nil {
        return "", "", err
    }
    return clientId, login, nil
}

hub := server.NewStompHubServer(
    server.WithAuthenticator(&tokenAuthenticator{verifier: v}),
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

**完成度**：命令与语义层已对齐上游 v3.2.0，中继状态的所有权也与上游同构（见下面第二小节）。

### 与上游 v3.2.0 的对齐

v3.1.5 → v3.2.0 共 42 个提交，绝大多数落在**客户端**（新增 `SubscribeOpt.Receipt`、`Unsubscribe` 改用 `done` channel、
`Ack` nil 校验、心跳双方协商、ID 从进程级改为连接级 `Conn.AllocateID`）与**工程治理**（移除 `stompd`、去 gocheck 换
testify）。`server/queue/*` 与 `server/topic/*` 的源码在这两个版本之间**零改动**。

落在服务端、因而需要同步的只有三处，现已全部对齐：

1. **错误不再静默丢弃** —— 上游把 `queue.Subscribe` / `Enqueue` / `Requeue` 的返回值从丢弃改为记日志；本实现同样接出这些
   错误，并且在 `SubscribeToDestination` 里把 queue 的订阅错误**返回给调用方** —— 订阅失败时不再照发 RECEIPT 谎报成功。
2. **帧编码错误上抛** —— v3.2.0 起 `frame.Writer` 会在写 CONNECT/CONNECTED 之前校验未转义的头，非法头值返回
   `ErrInvalidFrameFormat`。本实现的 `frameToBytes` 现在把该错误交出来并断开连接，而不是静默发出一个空帧。
3. **`netErr.Temporary()` → `netErr.Timeout()`** —— 本实现走 `http.Server`，没有自己的 accept 循环，不适用。

**面向客户端的行为变化**：按 STOMP 1.2，CONNECT 与 CONNECTED 的头**不做转义**（v3.1.5 对所有帧一律转义）。本实现直接复用
`frame` 包，该变化随依赖升级自动生效：

- 读侧：客户端 CONNECT 里 `login` / `passcode` 含 `\` 或 `\c` 的值现在按字面保留 —— 以前会被反转义改写，属凭证失真
- 写侧：服务端发出的 CONNECTED 头不再转义，与手写裸帧的客户端（如 `web/client.html`）一致

### 中继状态的所有权（与上游同构）

中继（topic / queue）的状态由**单个 goroutine 独占**：所有 `Manager` / `Topic` / `Queue` 的读写都
发生在 `server/processor.go` 的 `requestProcessor` 循环里，各连接协程与外部调用方只能通过
`chan Request` 提交请求。因此 `server/topic`、`server/queue` 里的裸 `map` 与 `container/list`
**不需要任何锁** —— 与上游 `server/processor.go` 同构。

这两个包的 9 个源文件里 7 个与上游逐字一致；`server/queue/queue.go` 改过（把 `*client.Subscription`
换成 `queue.Subscription` 接口），`server/queue/subscription.go` 与 `server/queue/subscription_list.go`
是本地 shim（上游 `server/queue/` 里没有这两个文件，上游直接复用 `client` 包的同名类型）。

（`SubscriptionList` 不在「中继独占」之列：它是每连接的对象，由连接的 `subListMu` 保护 —— 上游只在
processLoop 一个协程里用它，本实现把写出拆到了 writeLoop，注册与 ACK 就分属两个协程了。）

### 运行时行为（几条容易踩的）

| 行为 | 说明 |
|---|---|
| **心跳** | 按 STOMP 规范发**数据帧心跳**（一个 `"\n"` 文本帧），不是 WebSocket 控制帧 ping —— 主流客户端（stompjs）只用数据帧刷新「服务端活跃时间」，发 ping 会被它判成心跳丢失并主动断开空闲连接。间隔 = CONNECT 协商出的 `heart-beat`，下限 30s |
| **关停** | `main.go` 接 `SIGINT`/`SIGTERM` → 停 HTTP（不再接新连接）→ `StompHubServer.Shutdown(10s)`：通知各连接收尾、把未确认的队列帧退回队首、关掉 PresenceManager。启动失败（如端口被占）以非 0 退出 |
| **慢消费者** | 投递走每连接缓冲（`subChannel`/`writeChannel` 各 256），**不进中继循环**；缓冲满即判慢消费者并断开该连接，队列消息退回队首不丢 |
| **队列的投递语义** | 正常投递 + ack 模式确认；连接异常断开时，未确认的帧退回队首 —— 因此 `/queue/` 的 `ack` 模式是「至少一次」（可能重复），`ack:auto` 在异常断开时同样按至少一次处理 |
| **目的地上限** | 目的地由客户端任意构造，而 Manager 只增不删，所以中继对**新建**目的地设上限（默认 4096，见 `maxDestinations`），到上限后拒绝新建、已存在的照常服务 |
| **连接上限 / 空连接超时** | 同时连接数上限默认 10000（`maxConnections`，0 为不限）；连上之后 `connectTimeout`（默认 10s）内没发 CONNECT 即断开 —— 两者都是为了让未认证客户端无法稳定占住「每连接 3 协程 + 通道缓冲」 |
| **CONNECT 校验** | CONNECT/STOMP 帧本身也过验证器（必需 `host`/`accept-version`、`receipt` 不允许、`heart-beat` 格式与上限），失败回 ERROR |
| **独立运行的聊天** | `main.go` 用 `DefaultEventHook`，它**不路由** SEND —— 所以只跑 `stompws` 时 SEND 不会广播给任何人。要让测试页真的能聊，需要自定义 `EventHook` 在 `OnSend` 里构造 MESSAGE 帧并调 `RouteMessage`（`blog-cloud` 的 `stomphook` 就是这么做的） |

有两点与上游不同，都是刻意的：

1. **订阅类请求带回执**。上游的 `handleSubscribe` 是 fire-and-forget，且它的服务端**不回应
   SUBSCRIBE 的 RECEIPT**（`stompd` 因此与上游自己 v3.2.0 新增的 `SubscribeOpt.Receipt` 不兼容）。
   本实现等中继真正挂上订阅之后再回 RECEIPT —— 这条时序是 `SubscribeOpt.Receipt` 所依赖的契约，
   有测试守着。
2. **投递不阻塞中继循环**。缓冲满时视为慢消费者：**断开该连接**；队列消息退回队首不丢
   （代价是 ack:auto 的队列订阅变成「至少一次」语义，异常断开时可能重复）。上游是阻塞式投递，
   一个慢客户端会拖住整个中继 —— 也就是所有连接。

前述「并发写 map 导致 `fatal error`」的缺陷已由这次改造修掉。缺陷记录与三种修法的取舍见仓库根
`docs/` 下的 `bugs/2026-10-15-stompws中继状态并发缺失.md`，改造方案见 `docs/feature/2026-10-15-stompws中继串行化改造.md`。

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
	"github.com/ve-weiyi/stompws/server"
	"github.com/ve-weiyi/stompws/logws"
)

func main() {
	// 创建服务器
	hub := server.NewStompHubServer(
		server.WithEventHooks(server.NewDefaultEventHook()),
		server.WithAuthenticator(server.NewNoAuthenticator()),
		server.WithLogger(logws.NewDefaultLogger()),
	)

	// 注册 WebSocket 路由
	http.HandleFunc("/admin-api/v1/websocket", hub.HandleWebSocket)
	
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
hub := server.NewStompHubServer(
    server.WithAuthenticator(server.NewNoAuthenticator()),
)

// 自定义认证：实现 client.Authenticator 后注入（写法见上文「自定义认证」）
hub := server.NewStompHubServer(
    server.WithAuthenticator(&tokenAuthenticator{verifier: v}),
)
```

### 自定义事件钩子

```go
import "github.com/go-stomp/stomp/v3/frame"

type MyEventHook struct {
server.DefaultEventHook
}

func (h *MyEventHook) OnConnect(s *server.StompHubServer, c *client.Conn) {
// 用户连接时的自定义逻辑
}

func (h *MyEventHook) OnSend(s *server.StompHubServer, c *client.Conn, message *frame.Frame) bool {
// 消息发送前的拦截处理
return true // 返回 false 可阻止消息发送
}

hub := server.NewStompHubServer(
server.WithEventHooks(&MyEventHook{}),
)
```

### 自定义日志实现

```go
type MyLogger struct{}

func (l *MyLogger) Infof(format string, args ...interface{})    {}
func (l *MyLogger) Warningf(format string, args ...interface{}) {}
func (l *MyLogger) Errorf(format string, args ...interface{})   {}

hub := server.NewStompHubServer(
server.WithLogger(&MyLogger{}),
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
