// 本包的测试替身：queue.Storage 与 EventHook 的假实现、在线状态端口的计数实现。
package server

import (
	"context"
	"sync"
	"time"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
	"github.com/ve-weiyi/stompws/server/queue"
)

// fakeStorage 是 queue.Storage 的替身：Dequeue 可按需报错或 panic，Requeue 可按需阻塞，
// 并记录收到的帧。
//
// 多种注入挤在一个替身里，是因为这几个变体只差一个字段 —— 各自实现一遍 Storage 的五个
// 方法，会多出四份一字不差的空函数。
type fakeStorage struct {
	mu     sync.Mutex
	frames []*frame.Frame

	dequeueErr   error         // 非 nil 时 Dequeue 返回它，用于验证订阅错误会向上传递
	dequeuePanic bool          // Dequeue 必 panic，用于验证中继的协程兜底
	gate         chan struct{} // 非 nil 时 Requeue 阻塞到关闩，用来把中继卡在处理中
}

func (s *fakeStorage) Enqueue(string, *frame.Frame) error { return nil }

func (s *fakeStorage) Requeue(_ string, f *frame.Frame) error {
	if s.gate != nil {
		<-s.gate
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
	return nil
}

func (s *fakeStorage) Dequeue(string) (*frame.Frame, error) {
	if s.dequeuePanic {
		panic("storage panic for test")
	}
	return nil, s.dequeueErr
}

func (s *fakeStorage) Start() {}

func (s *fakeStorage) Stop() {}

func (s *fakeStorage) received() []*frame.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*frame.Frame(nil), s.frames...)
}

var _ queue.Storage = (*fakeStorage)(nil)

// ──────────────── 阶段 3：中继端到端 ────────────────

// routeHook 让 SEND 真的走一遍中继：默认 hook 不路由，只有消费方 hook 才会调
// RouteMessage（并像生产 hook 那样把 SEND 转成新的 MESSAGE 帧再交出去）。
//
// 返回 true（与生产 hook 一致）：OnSend 返回 false 的语义是**拒绝**这条消息，
// 服务端据此不回 RECEIPT。
type routeHook struct {
	DefaultEventHook
}

func (h *routeHook) OnSend(server *StompHubServer, c *client.Conn, message *frame.Frame) bool {
	msg := frame.New(frame.MESSAGE, frame.Destination, message.Header.Get(frame.Destination))
	msg.Body = message.Body
	server.RouteMessage(c, msg)
	return true
}

// passthroughHook 把**原始 SEND 帧**直接交中继（RouteMessage 会把命令强转成 MESSAGE）——
// 这是受支持的用法（上游的 handleSend 就这么干）。生产 hook 多自建 MESSAGE 帧，
// 所以这条路径上的头部卫生得单独钉住。
type passthroughHook struct {
	DefaultEventHook
}

func (h *passthroughHook) OnSend(server *StompHubServer, c *client.Conn, message *frame.Frame) bool {
	server.RouteMessage(c, message)
	return true
}

// panicHook 在指定回调上 panic，用来验证连接层与清理路径的兜底。
type panicHook struct {
	DefaultEventHook

	onSubscribe  bool
	onDisconnect bool
}

func (h *panicHook) OnSubscribe(*StompHubServer, *client.Conn, string, string) {
	if h.onSubscribe {
		panic("hook panic for test")
	}
}

func (h *panicHook) OnDisconnect(*StompHubServer, *client.Conn) {
	if h.onDisconnect {
		panic("hook panic for test")
	}
}

// rejectHook 拒绝所有 SEND。
type rejectHook struct {
	DefaultEventHook
}

func (h *rejectHook) OnSend(*StompHubServer, *client.Conn, *frame.Frame) bool { return false }

// countingPresence 记录上报次数与是否被关闭。
//
// 它按已知连接集合把 Add 分成「首报」与「续期」两类 —— 端口的契约就是 Add 幂等
// （重复调用即续期），替身照这份契约记账，两者才分得开。
type countingPresence struct {
	mu     sync.Mutex
	known  map[presenceKey]struct{}
	joins  int
	renews int
	closed bool
}

func (p *countingPresence) Add(_ context.Context, login, ref string, _ time.Duration) error {
	key := presenceKey{login: login, ref: ref}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.known == nil {
		p.known = make(map[presenceKey]struct{})
	}
	if _, ok := p.known[key]; ok {
		p.renews++
	} else {
		p.known[key] = struct{}{}
		p.joins++
	}
	return nil
}

func (p *countingPresence) Remove(_ context.Context, login, ref string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.known, presenceKey{login: login, ref: ref})
	return nil
}

func (p *countingPresence) PresenceStats(context.Context) (PresenceStats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PresenceStats{NumUsers: len(p.known), NumClients: len(p.known)}, nil
}

func (p *countingPresence) Presence(context.Context) ([]PresenceMember, error) { return nil, nil }

func (p *countingPresence) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *countingPresence) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func (p *countingPresence) joinCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.joins
}

func (p *countingPresence) renewCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.renews
}

var _ PresenceManager = (*countingPresence)(nil)

// recordingHook 记录各回调次数。OnSend 返回 true，好让后面的 routeHook 继续处理。
type recordingHook struct {
	DefaultEventHook

	mu           sync.Mutex
	connects     int
	subscribes   int
	unsubscribes int
	disconnects  int
	sends        int
}

func (h *recordingHook) OnConnect(*StompHubServer, *client.Conn) { h.bump(&h.connects) }

func (h *recordingHook) OnSubscribe(*StompHubServer, *client.Conn, string, string) {
	h.bump(&h.subscribes)
}

func (h *recordingHook) OnUnsubscribe(*StompHubServer, *client.Conn, string, string) {
	h.bump(&h.unsubscribes)
}

func (h *recordingHook) OnDisconnect(*StompHubServer, *client.Conn) { h.bump(&h.disconnects) }

func (h *recordingHook) OnSend(*StompHubServer, *client.Conn, *frame.Frame) bool {
	h.bump(&h.sends)
	return true
}

func (h *recordingHook) bump(counter *int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*counter++
}

func (h *recordingHook) count(counter *int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *counter
}
