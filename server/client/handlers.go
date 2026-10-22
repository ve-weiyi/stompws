package client

// handlers.go 装「客户端来的帧怎么被处理」：取帧、按命令分发、各命令实现与断开清理。

import (
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
)

// authFailedDelay 是认证失败后回 ERROR 之前的延时，用来拖慢爆破。
const authFailedDelay = time.Second

func (c *Conn) processLoop() {
	defer func() {
		if r := recover(); r != nil {
			c.log.Errorf("client=%s: processLoop panic: %v\nstack: %s", c.id, r, debug.Stack())
		}

		// 清理会调用消费方的 hook 与 tracker，它们的 panic 不能被上面的 recover 漏出去。
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.log.Errorf("client=%s: disconnect cleanup panic: %v\nstack: %s", c.id, r, debug.Stack())
				}
			}()
			c.cleanup()
		}()

		// 兜底关连接：不关就是僵尸连接（不再处理帧、也收不到消息，socket 却还开着）。
		c.close()
		// 连接计数归服务端（Serve 的 wg 由服务端记）。
		c.up.GoroutineDone()
	}()

	// 读超时由 readLoop 的读 deadline 管（读失败即结束本连接），这里无需看门狗。
	for {
		select {
		case f := <-c.readChannel:
			c.handleFrame(f)
		case <-c.up.Done():
			c.sendError(errServerShuttingDown, "")
			// 给 writeLoop 短暂时间发送 ERROR 帧
			select {
			case <-time.After(100 * time.Millisecond):
			case <-c.closeChan:
			}
			c.close()
			return
		case <-c.closeChan:
			return
		}
	}
}

func (c *Conn) handleFrame(f *frame.Frame) {
	// 验证帧
	if c.validator != nil {
		if err := c.validator.Validate(f); err != nil {
			c.log.Warningf("client=%s user=%s: validation failed for %s frame: %v", c.id, c.login, f.Command, err)
			c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
			return
		}
	}

	// 检查连接状态
	if !c.connected && f.Command != frame.CONNECT && f.Command != frame.STOMP {
		c.SendErrorAndClose(errNotConnected, "")
		return
	}

	if c.connected && (f.Command == frame.CONNECT || f.Command == frame.STOMP) {
		c.SendErrorAndClose(errAlreadyConnected, "")
		return
	}

	switch f.Command {
	case frame.CONNECT, frame.STOMP:
		c.handleConnect(f)
	case frame.DISCONNECT:
		c.handleDisconnect(f)
	case frame.SUBSCRIBE:
		c.handleSubscribe(f)
	case frame.SEND:
		c.handleSend(f)
	case frame.UNSUBSCRIBE:
		c.handleUnsubscribe(f)
	case frame.ACK:
		c.handleAck(f)
	case frame.NACK:
		c.handleNack(f)
	case frame.BEGIN:
		c.handleBegin(f)
	case frame.COMMIT:
		c.handleCommit(f)
	case frame.ABORT:
		c.handleAbort(f)
	default:
		c.SendErrorAndClose(errUnknownCommand, f.Header.Get(frame.Receipt))
	}
}

func (c *Conn) handleConnect(f *frame.Frame) {
	// 身份验证
	clientId, login, err := c.config.Authenticator.Authenticate(c, f)
	if err != nil {
		c.log.Errorf("authentication failed: %v", err)
		time.Sleep(authFailedDelay) // Prevent brute force
		c.SendErrorAndClose(err, "")
		return
	}

	if clientId == "" {
		clientId = c.ws.RemoteAddr().String()
	}
	if login == "" {
		login = clientId
	}
	c.id = clientId
	c.login = login

	// 版本协商。accept-version 对 CONNECT 与 STOMP 都是 REQUIRED，但「缺头视为 1.0」
	// 只适用于 CONNECT；STOMP 帧是 1.1 才引入的，缺头即非法。
	acceptVersion, _ := f.Header.Contains(frame.AcceptVersion)
	if acceptVersion == "" && f.Command == frame.STOMP {
		c.SendErrorAndClose(errMissingHeader(frame.AcceptVersion), "")
		return
	}
	version, err := negotiateVersion(acceptVersion)
	if err != nil {
		c.SendErrorAndClose(err, "")
		return
	}
	c.version = version

	// 心跳协商。cx 只用于 CONNECTED 的响应头；readLoop 自己另算一份读超时。
	cx, cy, err := parseHeartBeat(f.Header.Get(frame.HeartBeat))
	if err != nil {
		c.SendErrorAndClose(err, "")
		return
	}

	if cx > 0 && cx < c.config.MinHeartBeat {
		cx = c.config.MinHeartBeat
	}
	if cy > 0 && cy < c.config.MinHeartBeat {
		cy = c.config.MinHeartBeat
	}
	c.setWriteTimeout(cy)

	// 初始化验证器并立刻校验 CONNECT 本身（handleFrame 的校验在协商版本之前，首帧曾漏校验）。
	if err := version.CheckSupported(); err != nil {
		c.SendErrorAndClose(err, "")
		return
	}
	c.validator = newValidator(version)
	if err := c.validator.Validate(f); err != nil {
		c.log.Warningf("client=%s: validation failed for CONNECT frame: %v", c.GetIpAddress(), err)
		c.SendErrorAndClose(err, "")
		return
	}

	c.connected = true

	c.log.Infof("client=%s user=%s: connected, version=%s, heartbeat=%s", c.id, c.login, version, formatHeartBeat(cy, cx))

	response := frame.New(frame.CONNECTED,
		frame.Version, version.String(),
		frame.Session, c.id,
		frame.HeartBeat, formatHeartBeat(cy, cx))
	c.SendFrame(response)

	c.sendReceipt(f)

	// 会话正式建立：由服务端登记连接与在线用户并触发 OnConnect 钩子。
	c.up.Connected(c)
}

func (c *Conn) handleDisconnect(f *frame.Frame) {
	// 先发送 receipt
	c.sendReceipt(f)

	// 执行断开连接的清理逻辑
	c.cleanup()

	// 规范：客户端此后不得再发任何帧，故刷出 RECEIPT 即关闭，不留半个连接；
	// 订阅登记一并清掉，免得清理路径再看到它们（原文见规范要点 §五）。
	select {
	case <-time.After(100 * time.Millisecond):
	case <-c.closeChan:
	}
	c.subscriptions = make(map[string]*Subscription)
	c.close()
}

// cleanupClient 执行客户端断开连接时的清理工作
func (c *Conn) cleanup() {
	if !c.connected {
		return
	}

	// 清理事务
	c.txStore.Clear()

	// 退回未确认的帧：只扫 subList（帧已写出、仅未确认）—— c.subscriptions 里的帧可能还
	// 压在 subChan 里，读 sub.frame 会与 processor 竞争；未写出的由 writeLoop 退出时退回。
	// 锁内只摘取，交给中继放锁外（Requeue 会阻塞，而 writeLoop 的 Add 要同一把锁）。
	c.subListMu.Lock()
	var unacked []*frame.Frame
	for sub := c.subList.Get(); sub != nil; sub = c.subList.Get() {
		sub.active.Store(false)
		// 是不是队列帧由中继判（topic 是 fire-and-forget），这里只摘取。
		if sub.frame != nil {
			unacked = append(unacked, sub.frame)
		}
	}
	c.subListMu.Unlock()

	for _, f := range unacked {
		c.up.Requeue(f)
	}

	// 取消订阅
	for _, sub := range c.subscriptions {
		// 先置无效再交中继撤销：中继挂回队列池前会看它，途中任何重订阅都复活不了它。
		sub.active.Store(false)
		// 摘除订阅（OnUnsubscribe 由服务端在 Detach 内触发）。
		_ = c.up.Detach(sub)
	}

	c.log.Infof("client=%s user=%s: disconnected", c.id, c.login)

	// 注销登记并触发 OnDisconnect 钩子 —— 放在清理的最后，顺序不变量：
	// 未确认帧已退回 → 订阅已摘除 → 才宣告断开。
	c.up.Disconnected(c)

	c.connected = false
}

func (c *Conn) handleSubscribe(f *frame.Frame) {
	dest := f.Header.Get(frame.Destination)
	id := f.Header.Get(frame.Id)
	ack := f.Header.Get(frame.Ack)

	// 检查订阅冲突
	if _, exists := c.subscriptions[id]; exists {
		c.SendErrorAndClose(errSubscriptionExists, f.Header.Get(frame.Receipt))
		return
	}

	if ack == "" {
		ack = frame.AckAuto
	}

	sub := NewSubscription(c, dest, id, ack) // 新建即有效

	// 交中继成功后才登记到 c.subscriptions：反过来会让失败的 id 永久占住
	//（同 id 重试只会得到 exists），并留下一个从未挂上的幽灵订阅。
	if err := c.up.Attach(sub); err != nil {
		sub.active.Store(false)
		c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
		return
	}
	c.subscriptions[id] = sub
	// OnSubscribe 由服务端在 Attach 内触发（与「已挂上」同一处，顺序因而保持）。

	c.log.Infof("client=%s user=%s: subscribed to %s (ack=%s)", c.id, c.login, dest, ack)
	c.sendReceipt(f)
}

func (c *Conn) handleUnsubscribe(f *frame.Frame) {
	id := f.Header.Get(frame.Id)
	sub, ok := c.subscriptions[id]
	if !ok {
		c.SendErrorAndClose(errSubscriptionNotFound, "")
		return
	}

	delete(c.subscriptions, id)

	// 先置无效再交中继撤销：中继挂回队列池前会看它，途中任何重订阅都复活不了它。
	sub.active.Store(false)

	_ = c.up.Detach(sub)
	c.log.Infof("client=%s user=%s: unsubscribed from %s", c.id, c.login, sub.dest)

	// 先发送 receipt
	c.sendReceipt(f)

	// OnUnsubscribe 已由 Detach（服务端侧）触发。
}

func (c *Conn) handleSend(f *frame.Frame) {
	// 回执头先取走：接下来的 hook 可能把这一帧直接交给中继投递出去。
	receiptID, hasReceipt := takeReceipt(f)

	// 检查是否在事务中
	if tx, ok := f.Header.Contains(frame.Transaction); ok {
		// 先校验事务再回执，否则会先谎报成功、再回 ERROR（消息其实被丢了）。
		if err := c.txStore.Add(tx, f); err != nil {
			c.SendErrorAndClose(err, receiptID)
			return
		}
		if hasReceipt {
			c.sendReceiptID(receiptID)
		}
		return
	}

	// 交由服务端跑 OnSend 钩子：必须在**本协程**同步执行 —— 挪到中继协程后，慢钩子会卡死整个中继。
	// 钩子拒绝就不能回 RECEIPT —— 那等于把拒绝报成已受理，客户端因等不到回执而发现。
	if !c.up.Publish(c, f) {
		return
	}

	if hasReceipt {
		c.sendReceiptID(receiptID)
	}
}

func (c *Conn) handleAck(f *frame.Frame) {
	// 1.0/1.1 用 message-id 或 ack 回传；1.2 用 id（回传 MESSAGE 的 ack 头值）。
	ackId := f.Header.Get(frame.Ack)
	if ackId == "" {
		ackId = f.Header.Get(frame.MessageId)
	}
	if ackId == "" {
		ackId = f.Header.Get(frame.Id)
	}

	if ackId == "" {
		// 规范：ACK/NACK 必填 id；拿不到就无法处理这一帧 → ERROR + 关闭。
		c.SendErrorAndClose(errMissingHeader("id or message-id"), f.Header.Get(frame.Receipt))
		return
	}

	// 检查是否在事务中。先校验再回执：反过来会先谎报成功、再回 ERROR。
	if tx, ok := f.Header.Contains(frame.Transaction); ok {
		if err := c.txStore.Add(tx, f); err != nil {
			c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
			return
		}
		c.sendReceipt(f)
		return
	}

	msgId, err := stringToUint64(ackId)
	if err != nil {
		c.SendErrorAndClose(errNonNumericAckID(ackId), f.Header.Get(frame.Receipt))
		return
	}

	c.sendReceipt(f)

	// 上报 ACK，由服务端触发 OnAck 钩子。
	c.up.Acked(c, ackId)

	// 处理 ACK：锁内只摘取、重新订阅放锁外（Attach 会等中继确认，Add 要同一把锁）。
	c.subListMu.Lock()
	var resubscribe []*Subscription
	c.subList.Ack(msgId, func(sub *Subscription) {
		sub.frame = nil
		// subList 里只有队列订阅（它只由 deliverSub 那条队列路径填充），无需判目的地。
		resubscribe = append(resubscribe, sub)
	})
	c.subListMu.Unlock()

	// 重新订阅队列，传入Subscription对象。失败只记日志：ACK 不需要等它完成。
	for _, sub := range resubscribe {
		if err := c.up.Attach(sub); err != nil {
			c.log.Errorf("client=%s user=%s: resubscribe %s failed: %v", c.id, c.login, sub.dest, err)
		}
	}
}

func (c *Conn) handleNack(f *frame.Frame) {
	// 同 Ack：1.2 用 id。
	ackId := f.Header.Get(frame.Ack)
	if ackId == "" {
		ackId = f.Header.Get(frame.MessageId)
	}
	if ackId == "" {
		ackId = f.Header.Get(frame.Id)
	}

	// 检查是否在事务中。先校验再回执。
	if tx, ok := f.Header.Contains(frame.Transaction); ok {
		if err := c.txStore.Add(tx, f); err != nil {
			c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
			return
		}
		c.sendReceipt(f)
		return
	}

	msgId, err := stringToUint64(ackId)
	if err != nil {
		c.SendErrorAndClose(errNonNumericAckID(ackId), f.Header.Get(frame.Receipt))
		return
	}

	c.sendReceipt(f)

	// 上报 NACK，由服务端触发 OnNack 钩子。
	c.up.Nacked(c, ackId)

	// 处理 NACK：同 Ack，锁内只摘取（subList 里只有队列订阅，无需判目的地）。
	type pendingFrame struct {
		sub   *Subscription
		frame *frame.Frame
	}

	c.subListMu.Lock()
	var pendings []pendingFrame
	c.subList.Nack(msgId, func(sub *Subscription) {
		pendings = append(pendings, pendingFrame{sub: sub, frame: sub.frame})
		sub.frame = nil
	})
	c.subListMu.Unlock()

	for _, p := range pendings {
		// 先重新入队到队首，再把订阅挂回去 —— 顺序不能反，否则会先拿到下一条。
		if p.frame != nil {
			c.up.Requeue(p.frame)
		}
		if err := c.up.Attach(p.sub); err != nil {
			c.log.Errorf("client=%s user=%s: resubscribe %s failed: %v", c.id, c.login, p.sub.dest, err)
		}
	}
}

func (c *Conn) handleBegin(f *frame.Frame) {
	tx := f.Header.Get(frame.Transaction)
	if err := c.txStore.Begin(tx); err != nil {
		c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
		return
	}

	c.log.Infof("client=%s user=%s: transaction %s begun", c.id, c.login, tx)
	c.sendReceipt(f)
}

func (c *Conn) handleCommit(f *frame.Frame) {
	tx := f.Header.Get(frame.Transaction)

	// 提交事务，执行所有缓存的帧
	err := c.txStore.Commit(tx, func(cached *frame.Frame) error {
		// 兜底再摘一次 receipt 头：入库时 handleSend 已取走，重放不该再回一次。
		cached.Header.Del(frame.Receipt)

		// 重新处理每个帧
		c.handleFrame(cached)
		return nil
	})

	if err != nil {
		c.log.Errorf("client=%s user=%s: transaction %s commit failed: %v", c.id, c.login, tx, err)
		c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
		return
	}

	c.log.Infof("client=%s user=%s: transaction %s committed", c.id, c.login, tx)
	// 回执只能发在提交成功之后：RECEIPT 按规范是「已处理」且累积到此前所有帧，
	// 抢在提交前发就等于谎报提交成功（客户端随后才收到 ERROR，而事务并没有提交）。
	c.sendReceipt(f)
}

func (c *Conn) handleAbort(f *frame.Frame) {
	tx := f.Header.Get(frame.Transaction)
	if err := c.txStore.Abort(tx); err != nil {
		c.SendErrorAndClose(err, f.Header.Get(frame.Receipt))
		return
	}

	c.log.Infof("client=%s user=%s: transaction %s aborted", c.id, c.login, tx)
	c.sendReceipt(f)
}

// stringToUint64 解析 ack id。解析失败必须交出来：吞掉会得到 0，而
// IsAckedBy(0) 恒为 false，于是 ACK 被静默丢弃、该订阅一直卡在 subList。
func stringToUint64(s string) (uint64, error) {
	return strconv.ParseUint(s, 10, 64)
}
