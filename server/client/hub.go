package client

import "github.com/go-stomp/stomp/v3/frame"

// Hub 是服务端交给每条连接的中继句柄，也是连接层**唯一向上调用**的东西。
// 接口定义在消费方：目的地归属、钩子触发、会话登记都留在实现侧。
// **各方法的协程契约不同**，逐条写在方法上。
type Hub interface {
	// Attach 把订阅挂到它的目的地（初次订阅与「投递后挂回队列池」都走这里）。
	// 同步等中继确认；走请求通道，可在任意协程调用。
	Attach(sub *Subscription) error

	// Detach 把订阅从它的目的地摘掉。同步等中继确认；失败交出来。
	// 走中继请求通道，可在任意协程调用。
	Detach(sub *Subscription) error

	// Publish 受理一条 SEND：false = 应用拒绝（调用方不得回 RECEIPT）。钩子必须在
	// **调用方协程**上执行 —— 挪到中继协程后，慢钩子会卡死整个中继。
	Publish(c *Conn, f *frame.Frame) bool

	// Requeue 把未能送达的帧退回其目的地队首（topic 是 fire-and-forget，实现侧忽略）。
	// 走中继请求通道，可在任意协程调用。
	Requeue(f *frame.Frame)

	// RequeueDirect 同 Requeue，但**仅限中继协程内**调用（投递失败当场退回）。
	// 在其它协程调用它等于绕过请求通道，会与中继状态竞争。
	RequeueDirect(f *frame.Frame)

	// Acked / Nacked 上报一次确认/拒绝，由实现侧触发 OnAck / OnNack 钩子。
	Acked(c *Conn, messageID string)
	Nacked(c *Conn, messageID string)

	// —— 会话生命周期：连接层只上报，登记与钩子触发都在实现侧 ——

	// Connected 会话已建立：登记连接与在线用户，并触发 OnConnect 钩子。
	Connected(c *Conn)

	// Disconnected 会话已结束：注销登记、触发 OnDisconnect 钩子。
	// 调用时机在连接层清理的**最后**（未确认帧已退回、订阅已摘除）。
	Disconnected(c *Conn)

	// GoroutineDone 本连接的处理协程已退出，供实现侧回收连接计数。
	GoroutineDone()

	// Done 在中继/服务端开始关停时关闭 —— 连接层据此主动结束本连接。
	Done() <-chan struct{}
}
