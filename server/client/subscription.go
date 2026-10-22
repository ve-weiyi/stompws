package client

import (
	"sync/atomic"

	"github.com/go-stomp/stomp/v3/frame"
)

type Subscription struct {
	conn    *Conn
	dest    string
	id      string            // client's subscription id
	ack     string            // auto, client, client-individual
	msgId   uint64            // message-id (or ack) for acknowledgement
	subList *SubscriptionList // am I in a list
	frame   *frame.Frame      // message allocated to subscription

	// active 表示订阅仍有效：processLoop 置位/清除，writeLoop 写出前与中继挂回队列池前
	// 都要看它（退订与挂回分属两个协程，一个在途的重订阅就能把死订阅挂回去）。
	active atomic.Bool
}

func NewSubscription(c *Conn, dest string, id string, ack string) *Subscription {
	sub := &Subscription{
		conn: c,
		dest: dest,
		id:   id,
		ack:  ack,
	}
	sub.active.Store(true)
	return sub
}

func (s *Subscription) Conn() *Conn {
	return s.conn
}

func (s *Subscription) Destination() string {
	return s.dest
}

func (s *Subscription) Ack() string {
	return s.ack
}

func (s *Subscription) Id() string {
	return s.id
}

// IsActive 报告订阅是否仍有效（退订/断开后为 false），中继据此决定要不要挂回队列池。
func (s *Subscription) IsActive() bool {
	return s.active.Load()
}

// IsAckedBy 报告这条订阅是否被该 message-id 确认（按 ack 模式：累积 / 单独 / auto）。
func (s *Subscription) IsAckedBy(msgId uint64) bool {
	switch s.ack {
	case frame.AckAuto:
		return true
	case frame.AckClient:
		// any later message acknowledges an earlier message
		return msgId >= s.msgId
	case frame.AckClientIndividual:
		return msgId == s.msgId
	default:
		return false
	}
}

func (s *Subscription) IsNackedBy(msgId uint64) bool {
	return msgId == s.msgId
}

func (s *Subscription) SendQueueFrame(f *frame.Frame) {
	s.setSubscriptionHeader(f)
	s.frame = f

	// 非阻塞投递：中继循环不能被收得慢的客户端拖住，交不出去（缓冲满或已关）时当场退回
	// 队首。本方法只在 processor 协程上调用，故走 RequeueDirect —— 提交请求通道会自锁。
	if !s.conn.deliverSub(s) {
		s.frame = nil
		s.conn.up.RequeueDirect(f)
	}
}

func (s *Subscription) SendTopicFrame(f *frame.Frame) {
	s.setSubscriptionHeader(f)

	// let the connection deal with the subscription
	// acknowledgement
	s.conn.deliverFrame(f)
}

func (s *Subscription) setSubscriptionHeader(f *frame.Frame) {
	if s.frame != nil {
		panic("subscription already has a frame pending")
	}
	f.Header.Set(frame.Subscription, s.id)
}
