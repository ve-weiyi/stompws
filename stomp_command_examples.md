# STOMP 协议指令示例

WebSocket 端点：`ws://localhost:9091/admin-api/v1/websocket`

## 帧格式说明

每个 STOMP 帧由三部分组成：

```
命令\n
头部名:头部值\n
\n
消息体\x00
```

- 帧以命令行开头，每行以 `\n` 结尾
- 头部与消息体之间有一个空行
- 帧以 NULL 字节（`\x00`）结尾
- 无消息体时，空行后直接跟 `\x00`

---

## 1. 连接服务（CONNECT）

### 发送

```
CONNECT
accept-version:1.2
host:/
login:alice
passcode:password123

\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `accept-version` | 是 | 支持的协议版本，如 `1.0,1.1,1.2` |
| `host` | 是 | 虚拟主机，填 `/` |
| `login` | 否 | 用户名（启用认证时必填） |
| `passcode` | 否 | 密码（启用认证时必填） |
| `heart-beat` | 否 | 心跳设置，格式 `cx,cy`，单位毫秒 |
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 帧 |

### 响应

```
CONNECTED
version:1.2
session:127.0.0.1:54321
heart-beat:0,0

\x00
```

| 头部 | 说明 |
|------|------|
| `version` | 协商后的协议版本 |
| `session` | 服务端分配的会话 ID（客户端 ID） |
| `heart-beat` | 服务端心跳设置 |

---

## 2. 断开连接（DISCONNECT）

### 发送

```
DISCONNECT
receipt:receipt-001

\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 确认 |

### 响应（携带 receipt 时）

```
RECEIPT
receipt-id:receipt-001

\x00
```

---

## 3. 订阅目标（SUBSCRIBE）

### 发送

```
SUBSCRIBE
destination:/topic/pipeline/events
id:sub-1779332290672

\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `destination` | 是 | 订阅目标，支持 `/topic/*` 和 `/queue/*` |
| `id` | 是 | 订阅 ID，用于后续取消订阅和消息路由 |
| `ack` | 否 | 确认模式：`auto`（默认）、`client`、`client-individual` |
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 确认 |

无响应（未携带 receipt 时）。

### 目标类型

| 目标前缀 | 模式 | 说明 |
|----------|------|------|
| `/topic/*` | 广播 | 所有订阅者都能收到消息 |
| `/queue/*` | 点对点 | 消息只投递给一个订阅者（负载均衡） |

---

## 4. 取消订阅（UNSUBSCRIBE）

### 发送

```
UNSUBSCRIBE
id:sub-1779332290672

\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | 订阅时使用的 ID |
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 确认 |

无响应（未携带 receipt 时）。

---

## 5. 发送消息（SEND）

### 5.1 广播到 Topic

```
SEND
destination:/topic/chat

{"username":"alice","content":"Hello everyone!","timestamp":"2026-05-21T10:00:00Z"}
\x00
```

### 5.2 发送到 Queue

```
SEND
destination:/queue/orders

{"orderId":"12345","item":"book","qty":2}
\x00
```

### 5.3 私信用户

通过 `/user/{username}` 目标发送私信（需服务端 hook 支持路由）：

```
SEND
destination:/user/bob

{"username":"alice","content":"Hi Bob, this is private!","timestamp":"2026-05-21T10:00:00Z"}
\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `destination` | 是 | 消息目标 |
| `content-type` | 否 | 消息体类型，如 `application/json` |
| `transaction` | 否 | 事务 ID，在事务中发送时填写 |
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 确认 |

---

## 6. 接收消息（MESSAGE）

订阅目标后，服务端推送消息时客户端收到：

```
MESSAGE
destination:/topic/chat
message-id:42
subscription:sub-1779332290672

{"username":"bob","content":"Hello!","timestamp":"2026-05-21T10:00:01Z"}
\x00
```

| 头部 | 说明 |
|------|------|
| `destination` | 消息来源目标 |
| `message-id` | 消息唯一 ID，ACK/NACK 时使用 |
| `subscription` | 对应的订阅 ID |

---

## 7. 消息确认（ACK / NACK）

仅在订阅时 `ack` 不为 `auto` 时需要手动确认。

### 7.1 ACK（确认处理成功）

```
ACK
id:42

\x00
```

### 7.2 NACK（拒绝，重新入队）

```
NACK
id:42

\x00
```

| 头部 | 必填 | 说明 |
|------|------|------|
| `id` | 是 | MESSAGE 帧中的 `message-id` |
| `transaction` | 否 | 事务 ID |
| `receipt` | 否 | 填写后服务端会返回 RECEIPT 确认 |

NACK 后，Queue 消息会重新入队首，等待下次投递。

---

## 8. 事务（BEGIN / COMMIT / ABORT）

事务将多个 SEND / ACK / NACK 操作打包为原子操作。

### 8.1 提交事务

```
BEGIN
transaction:tx-1716278400000

\x00
```

```
SEND
destination:/queue/orders
transaction:tx-1716278400000

{"orderId":"001","item":"book"}
\x00
```

```
SEND
destination:/queue/notifications
transaction:tx-1716278400000

{"msg":"order placed"}
\x00
```

```
COMMIT
transaction:tx-1716278400000

\x00
```

### 8.2 回滚事务

```
ABORT
transaction:tx-1716278400000

\x00
```

| 指令 | 头部 | 必填 | 说明 |
|------|------|------|------|
| `BEGIN` | `transaction` | 是 | 事务 ID，自定义唯一字符串 |
| `COMMIT` | `transaction` | 是 | 提交，执行所有缓存操作 |
| `ABORT` | `transaction` | 是 | 回滚，丢弃所有缓存操作 |

---

## 9. RECEIPT 机制

在任意客户端帧中添加 `receipt` 头部，服务端处理完成后返回：

### 请求（以 SUBSCRIBE 为例）

```
SUBSCRIBE
destination:/topic/chat
id:sub-001
receipt:receipt-subscribe-001

\x00
```

### 响应

```
RECEIPT
receipt-id:receipt-subscribe-001

\x00
```

---

## 10. ERROR 响应

当服务端遇到错误时主动推送：

```
ERROR
message:subscription already exists
receipt-id:receipt-001

\x00
```

| 头部 | 说明 |
|------|------|
| `message` | 错误描述 |
| `receipt-id` | 触发错误的请求 receipt ID（如有） |

常见错误：

| 错误信息 | 原因 |
|----------|------|
| `subscription already exists` | 相同 ID 重复订阅 |
| `subscription not found` | 取消订阅时 ID 不存在 |
| `invalid destination` | 目标不以 `/topic/` 或 `/queue/` 开头 |
| `authentication failed` | 用户名或密码错误 |
| `transaction already exists` | 相同 ID 重复开启事务 |
| `transaction not found` | COMMIT/ABORT 时事务 ID 不存在 |
