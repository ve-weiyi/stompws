package server

// hub.go 实现连接层定义的 Hub 接口：这条「连接层 → 服务端」的唯一通道。
// 目的地归属判定、钩子触发、会话登记都在这里（连接层不认识它们）。
//
// 应用层的两个同名入口（SubscribeToDestination / UnsubscribeFromDestination）
// 是 Attach / Detach 的别名，也放在这里以免两处各写一份。

import (
	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
)

// SubscribeToDestination handles subscription based on destination type.
// 等中继真正挂上才返回，调用方因此可以安全地在返回之后回 RECEIPT。
func (s *StompHubServer) SubscribeToDestination(sub *client.Subscription) error {
	return s.Attach(sub)
}

// UnsubscribeFromDestination handles unsubscription based on destination type.
// 同样等中继确认。
func (s *StompHubServer) UnsubscribeFromDestination(sub *client.Subscription) error {
	return s.Detach(sub)
}

var _ client.Hub = (*StompHubServer)(nil)

// Attach 把订阅挂到它的目的地（初次订阅与投递后挂回队列池都走这里）。
// OnSubscribe 在这里触发：与「已挂上」同一处，且早于客户端收到 RECEIPT。
func (s *StompHubServer) Attach(sub *client.Subscription) error {
	if err := s.proc.call(SubscribeOp, sub); err != nil {
		return err
	}

	for _, hook := range s.eventHooks {
		hook.OnSubscribe(s, sub.Conn(), sub.Destination(), sub.Id())
	}
	return nil
}

// Detach 把订阅从它的目的地摘掉，并触发 OnUnsubscribe。
func (s *StompHubServer) Detach(sub *client.Subscription) error {
	if err := s.proc.call(UnsubscribeOp, sub); err != nil {
		return err
	}

	for _, hook := range s.eventHooks {
		hook.OnUnsubscribe(s, sub.Conn(), sub.Destination(), sub.Id())
	}
	return nil
}

// Publish 按 STOMP 的 SEND 处理：跑 OnSend 钩子，任一钩子返回 false 即视为应用拒绝。
// 钩子在**调用方协程**上执行（连接层不允许把它挪到中继协程：钩子可能落库或发起外部调用）。
func (s *StompHubServer) Publish(c *client.Conn, f *frame.Frame) bool {
	for _, hook := range s.eventHooks {
		if !hook.OnSend(s, c, f) {
			id, login, _ := c.GetSessionInfo()
			s.log.Warningf("client=%s user=%s: SEND to %s rejected by hook", id, login, f.Header.Get(frame.Destination))
			return false
		}
	}
	return true
}

// Requeue 把未能送达的帧退回其目的地队首（任意协程，走中继请求通道）。
func (s *StompHubServer) Requeue(f *frame.Frame) {
	if err := s.proc.submit(Request{Op: RequeueOp, Frame: f}); err != nil {
		s.log.Errorf("requeue to %s failed: %v", f.Header.Get(frame.Destination), err)
	}
}

// RequeueDirect 同 Requeue，但仅限中继协程内调用（投递失败当场退回）。
func (s *StompHubServer) RequeueDirect(f *frame.Frame) {
	s.proc.requeueDirect(f.Header.Get(frame.Destination), f)
}

func (s *StompHubServer) Acked(c *client.Conn, messageID string) {
	for _, hook := range s.eventHooks {
		hook.OnAck(c, messageID)
	}
}

func (s *StompHubServer) Nacked(c *client.Conn, messageID string) {
	for _, hook := range s.eventHooks {
		hook.OnNack(c, messageID)
	}
}

// Connected 会话已建立：登记连接与在线状态，并触发 OnConnect 钩子。
func (s *StompHubServer) Connected(c *client.Conn) {
	id, login, _ := c.GetSessionInfo()
	s.clients.Store(id, clientEntry{conn: c, login: login})
	if err := s.presence.Add(s.presenceCtx, login, id, s.presenceTTL); err != nil {
		// 在线状态不是关键路径：报不上去也不该让一个已协商成功的连接断掉。
		s.log.Warningf("presence add %s (ref %s) failed: %v", login, id, err)
	}

	for _, hook := range s.eventHooks {
		hook.OnConnect(s, c)
	}
}

// Disconnected 会话已结束：注销登记并触发 OnDisconnect 钩子。
// 连接层在清理的最后一步调用它（未确认帧已退回、订阅已摘除）。
func (s *StompHubServer) Disconnected(c *client.Conn) {
	id, login, _ := c.GetSessionInfo()
	s.clients.Delete(id)
	if err := s.presence.Remove(s.presenceCtx, login, id); err != nil {
		s.log.Warningf("presence remove %s (ref %s) failed: %v", login, id, err)
	}

	for _, hook := range s.eventHooks {
		hook.OnDisconnect(s, c)
	}
}

// GoroutineDone 本连接的处理协程已退出：回收连接计数。
func (s *StompHubServer) GoroutineDone() {
	s.connections.Add(-1)
}

// Done 服务端开始关停时关闭。
func (s *StompHubServer) Done() <-chan struct{} {
	return s.ctx.Done()
}
