package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/logws"
	"github.com/ve-weiyi/stompws/server/client"
	"github.com/ve-weiyi/stompws/server/queue"
)

// clientEntry 是连接表的一行：连接本身，以及它的会话身份。
//
// login 在这里存一份而不去 Conn 上现取，是因为取会话身份的接口只允许在连接自己的
// 回调协程里调用 —— 而续期协程要遍历本表。
type clientEntry struct {
	conn  *client.Conn
	login string
}

type StompHubServer struct {
	clients sync.Map // clientId -> clientEntry

	// connections 是当前连接数，配合 maxConnections 兜住资源占用。
	connections atomic.Int64

	// 中继状态的唯一执行者。topic/queue 的 manager 由它独占，hub 不再直接持有。
	proc         *requestProcessor
	queueStorage queue.Storage

	upgrader      websocket.Upgrader
	log           logws.Logger
	authenticator client.Authenticator
	eventHooks    []EventHook
	presence      PresenceManager

	// 以下参数一律经 WithXxx 注入，不做包级可变状态：包级量会让测试只能串行
	// （谁都能改），生产侧也无从配置。默认值在构造时给出。
	connectTimeout     time.Duration // 连上但一直不发 CONNECT 的容忍时长
	minHeartBeat       time.Duration // 协商心跳间隔的下限
	maxWSMessageSize   int64         // 单条 WebSocket 消息的大小上限
	maxConnections     int64         // 同时连接数上限（0 不限）
	maxDestinations    int           // 中继允许创建的目的地数上限
	onlineSyncInterval time.Duration // 在线名单续期的间隔
	presenceTTL        time.Duration // 在线条目的存活时长；必须显著大于续期间隔

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// presenceCtx 只给在线状态的读写用，且活得比连接久：Shutdown 一开始就取消 ctx，
	// 而各连接要到随后清理时才上报 Remove —— 用 ctx 会让断开路径的摘除全部失败。
	presenceCtx    context.Context
	presenceCancel context.CancelFunc
}

type ServerOption func(*StompHubServer)

// WithConnectTimeout 设置「WebSocket 连上但一直不发 CONNECT」的容忍时长。
func WithConnectTimeout(d time.Duration) ServerOption {
	return func(s *StompHubServer) {
		s.connectTimeout = d
	}
}

// WithMinHeartBeat 设置协商心跳间隔的下限：客户端要的更短时，服务端坚持这个值
// （抬高自己声明的值，规范允许）。
func WithMinHeartBeat(d time.Duration) ServerOption {
	return func(s *StompHubServer) {
		s.minHeartBeat = d
	}
}

// WithMaxWSMessageSize 设置单条 WebSocket 消息的大小上限。
func WithMaxWSMessageSize(n int64) ServerOption {
	return func(s *StompHubServer) {
		s.maxWSMessageSize = n
	}
}

// WithMaxConnections 设置同时连接数上限（0 不限）。未认证客户端也能建立 WebSocket，
// 不设限就是一条稳定的资源消耗路径（每连接三条协程 + 通道缓冲）。
func WithMaxConnections(n int64) ServerOption {
	return func(s *StompHubServer) {
		s.maxConnections = n
	}
}

// WithMaxDestinations 设置中继允许创建的目的地数上限（Manager 只增不删，
// 而目的地由客户端任意构造）。
func WithMaxDestinations(n int) ServerOption {
	return func(s *StompHubServer) {
		s.maxDestinations = n
	}
}

// WithOnlineSyncInterval 设置在线名单续期的间隔。
func WithOnlineSyncInterval(d time.Duration) ServerOption {
	return func(s *StompHubServer) {
		s.onlineSyncInterval = d
	}
}

// WithPresenceTTL 设置在线条目的存活时长。它应当显著大于续期间隔 ——
// 停发续期到条目消失之间，就是掉线（含节点崩溃）被察觉的窗口。
func WithPresenceTTL(d time.Duration) ServerOption {
	return func(s *StompHubServer) {
		s.presenceTTL = d
	}
}

func WithAuthenticator(auth client.Authenticator) ServerOption {
	return func(s *StompHubServer) {
		s.authenticator = auth
	}
}

func WithEventHooks(handlers ...EventHook) ServerOption {
	return func(s *StompHubServer) {
		s.eventHooks = append(s.eventHooks, handlers...)
	}
}

func WithLogger(logger logws.Logger) ServerOption {
	return func(s *StompHubServer) {
		s.log = logger
	}
}

func WithQueueStorage(storage queue.Storage) ServerOption {
	return func(s *StompHubServer) {
		s.queueStorage = storage
	}
}

// WithPresenceManager 换掉在线状态的存储后端（默认是本节点内存里的那份）。
func WithPresenceManager(p PresenceManager) ServerOption {
	return func(s *StompHubServer) {
		s.presence = p
	}
}

// Presence 取在线状态的存储端口，用于业务侧查名单与计数。
func (s *StompHubServer) Presence() PresenceManager {
	return s.presence
}

func WithCheckOrigin(checkOrigin func(*http.Request) bool) ServerOption {
	return func(s *StompHubServer) {
		s.upgrader.CheckOrigin = checkOrigin
	}
}

func NewStompHubServer(opts ...ServerOption) *StompHubServer {
	s := &StompHubServer{
		queueStorage: queue.NewMemoryQueueStorage(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			Subprotocols: []string{"stomp", "mqtt", "v12.stomp", "v11.stomp", "v10.stomp"},
		},
		log:           logws.NewDefaultLogger(),
		authenticator: NewNoAuthenticator(),
		eventHooks:    []EventHook{NewDefaultEventHook()},
		presence:      NewMemoryPresenceManager(),

		connectTimeout:     10 * time.Second,
		minHeartBeat:       30 * time.Second,
		maxWSMessageSize:   1 << 20,
		maxConnections:     10000,
		maxDestinations:    4096,
		onlineSyncInterval: 15 * time.Second,
		presenceTTL:        60 * time.Second,
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.presenceCtx, s.presenceCancel = context.WithCancel(context.Background())

	for _, opt := range opts {
		opt(s)
	}

	// 中继要在所有 option 应用完之后构造（WithQueueStorage 只记录 storage）。
	s.proc = newRequestProcessor(s.queueStorage, s.log, s.maxDestinations)
	go s.proc.serve()

	go s.presenceSyncLoop()

	return s
}

func (s *StompHubServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 关停期间拒绝新连接
	select {
	case <-s.ctx.Done():
		http.Error(w, errServerShuttingDown.Error(), http.StatusServiceUnavailable)
		return
	default:
	}

	n := s.connections.Add(1)
	if s.maxConnections > 0 && n > s.maxConnections {
		s.connections.Add(-1)
		s.log.Warningf("refusing connection from %s: %d connections (max %d)", r.RemoteAddr, n-1, s.maxConnections)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Errorf("websocket upgrade failed: %v", err)
		s.connections.Add(-1)
		return
	}
	// 规范允许服务端对帧大小设限（超过即回 ERROR 并关闭）。这里限制单条 WebSocket
	// 消息的大小，避免未认证客户端用超大消息占内存。
	conn.SetReadLimit(s.maxWSMessageSize)

	c := client.NewConn(client.Config{
		Log:            s.log,
		Authenticator:  s.authenticator,
		ConnectTimeout: s.connectTimeout,
		MinHeartBeat:   s.minHeartBeat,
	}, conn, s)
	s.log.Infof("new connection from %s", conn.RemoteAddr())

	// 每条连接只占一个 wg 槽：Serve 内部起三条协程，并在它们全部退出后返回。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		c.Serve()
	}()
}

// Shutdown 优雅关闭服务器，等待所有连接处理完毕后退出。
// timeout 指定等待的最长时间，超时后强制返回。
func (s *StompHubServer) Shutdown(timeout time.Duration) error {
	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	var err error
	select {
	case <-done:
		s.log.Infof("server shutdown complete")
	case <-time.After(timeout):
		s.log.Warningf("server shutdown timeout after %v", timeout)
		err = fmt.Errorf("shutdown timeout after %v", timeout)
	}

	// 中继要活到连接清理完毕：cleanupClient 还要往它提交重入队。
	s.proc.stop()

	// 等中继处理完已受理的请求再返回；stop 之后的新提交会拿到 ErrBrokerClosed。
	select {
	case <-s.proc.stopped:
	case <-time.After(timeout):
		s.log.Warningf("broker did not stop within %v", timeout)
		if err == nil {
			err = fmt.Errorf("broker did not stop within %v", timeout)
		}
	}

	// 连接已清理完毕，在线状态的读写到此为止；再关掉它自身持有的资源（连接、后台协程）。
	s.presenceCancel()
	if cerr := s.presence.Close(); cerr != nil {
		s.log.Warningf("closing presence manager failed: %v", cerr)
	}

	return err
}

// RouteMessage routes a message based on its destination.
//
// 投递是异步的：返回时消息只进了中继队列。传入的应当是 MESSAGE 帧，传了别的命令会被
// 改成 MESSAGE 并记告警；目的地非法时有 from 就同步回提示，否则记日志。
func (s *StompHubServer) RouteMessage(from *client.Conn, msg *frame.Frame) {
	if msg.Command != frame.MESSAGE {
		s.log.Warningf("RouteMessage got a %s frame, expected MESSAGE: coercing", msg.Command)
		msg.Command = frame.MESSAGE
	}

	dest := msg.Header.Get(frame.Destination)
	switch {
	case isTopicDestination(dest), isQueueDestination(dest):
		if err := s.proc.submit(Request{Op: EnqueueOp, Frame: msg}); err != nil {
			s.log.Errorf("enqueue to %s failed: %v", dest, err)
		}
	default:
		// 规范：服务端无法处理这条 SEND 时回 ERROR（而不是伪造一条 MESSAGE ——
		// MESSAGE 按规范必须带 subscription，而客户端根本没订阅那个目的地）。
		if from != nil {
			from.SendErrorAndClose(errInvalidDestinationFor(dest), "")
			return
		}
		s.log.Errorf("dropping message to invalid destination %q", dest)
	}
}

// ──────────────── built-in online tracking ────────────────

// presenceSyncLoop 周期性把「还连着的连接」重报一遍 —— 对幂等的 Add 来说就是续期。
// 续期间隔与条目存活时长（presenceTTL）的关系见 WithPresenceTTL。
func (s *StompHubServer) presenceSyncLoop() {
	ticker := time.NewTicker(s.onlineSyncInterval)
	defer ticker.Stop()

	// 必须监听 ctx：否则每建/销一个 hub 泄漏一个协程，且会在关停后继续上报。
	for {
		select {
		case <-ticker.C:
			s.clients.Range(func(key, value interface{}) bool {
				entry, ok := value.(clientEntry)
				if !ok {
					return true
				}
				if err := s.presence.Add(s.presenceCtx, entry.login, key.(string), s.presenceTTL); err != nil {
					s.log.Warningf("presence renew %s failed: %v", entry.login, err)
				}
				return true
			})
		case <-s.ctx.Done():
			return
		}
	}
}
