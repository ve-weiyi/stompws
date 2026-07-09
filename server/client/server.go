package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/logws"
	"github.com/ve-weiyi/stompws/server/queue"
	"github.com/ve-weiyi/stompws/server/topic"
)

type StompHubServer struct {
	clients    sync.Map // clientId -> *Client
	onlineList sync.Map // login -> clientId

	topicManager  *topic.Manager
	queueManager  *queue.Manager
	upgrader      websocket.Upgrader
	log           logws.Logger
	authenticator Authenticator
	eventHooks    []EventHook
	onlineTracker OnlineTracker

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type ServerOption func(*StompHubServer)

func WithAuthenticator(auth Authenticator) ServerOption {
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
		s.queueManager = queue.NewManager(storage)
	}
}

func WithOnlineTracker(tracker OnlineTracker) ServerOption {
	return func(s *StompHubServer) {
		s.onlineTracker = tracker
	}
}

func (s *StompHubServer) OnlineTracker() OnlineTracker {
	return s.onlineTracker
}

func WithCheckOrigin(checkOrigin func(*http.Request) bool) ServerOption {
	return func(s *StompHubServer) {
		s.upgrader.CheckOrigin = checkOrigin
	}
}

func NewStompHubServer(opts ...ServerOption) *StompHubServer {
	s := &StompHubServer{
		topicManager: topic.NewManager(),
		queueManager: queue.NewManager(queue.NewMemoryQueueStorage()),
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
		onlineTracker: NewMemoryOnlineTracker(),
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())

	for _, opt := range opts {
		opt(s)
	}

	s.onlineTracker.SetHub(s)
	go s.onlineTrackerSyncLoop()

	return s
}

func (s *StompHubServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 关停期间拒绝新连接
	select {
	case <-s.ctx.Done():
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	default:
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Errorf("websocket upgrade failed: %v", err)
		return
	}

	c := newClient(conn)
	c.log = s.log
	s.log.Infof("new connection from %s", conn.RemoteAddr())

	s.wg.Add(1)
	go c.readLoop()
	go c.writeLoop(s)
	go c.processLoop(s)
}

func (s *StompHubServer) disconnect(c *Client) {
	// 执行清理逻辑
	s.cleanupClient(c)
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

	select {
	case <-done:
		s.log.Infof("server shutdown complete")
		return nil
	case <-time.After(timeout):
		s.log.Warningf("server shutdown timeout after %v", timeout)
		return fmt.Errorf("shutdown timeout after %v", timeout)
	}
}

// RouteMessage routes a message based on its destination
func (s *StompHubServer) RouteMessage(from *Client, msg *frame.Frame) {
	dest := msg.Header.Get(frame.Destination)
	switch {
	case strings.HasPrefix(dest, "/topic/"):
		s.topicManager.Find(dest).Enqueue(msg)
	case strings.HasPrefix(dest, "/queue/"):
		s.queueManager.Find(dest).Enqueue(msg)
	default:
		if from != nil {
			errorMsg := frame.New(frame.MESSAGE, frame.Destination, "/topic/system", frame.MessageId, "0")
			errorMsg.Body = []byte(fmt.Sprintf(`{"username":"System","content":"Invalid destination: %s"}`, dest))
			from.SendFrame(errorMsg)
		}
	}
}

// SubscribeToDestination handles subscription based on destination type
func (s *StompHubServer) SubscribeToDestination(sub *Subscription) error {
	dest := sub.Destination()
	switch {
	case strings.HasPrefix(dest, "/topic/"):
		s.topicManager.Find(dest).Subscribe(sub)
	case strings.HasPrefix(dest, "/queue/"):
		s.queueManager.Find(dest).Subscribe(sub)
	default:
		return errInvalidDestination
	}
	return nil
}

// UnsubscribeFromDestination handles unsubscription based on destination type
func (s *StompHubServer) UnsubscribeFromDestination(sub *Subscription) error {
	dest := sub.Destination()
	switch {
	case strings.HasPrefix(dest, "/topic/"):
		s.topicManager.Find(dest).Unsubscribe(sub)
	case strings.HasPrefix(dest, "/queue/"):
		s.queueManager.Find(dest).Unsubscribe(sub)
	default:
		return errInvalidDestination
	}
	return nil
}

// ──────────────── built-in online tracking ────────────────

func (s *StompHubServer) onlineTrackerSyncLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.onlineList.Range(func(key, _ interface{}) bool {
			if login, ok := key.(string); ok {
				s.onlineTracker.OnActive(login)
			}
			return true
		})
	}
}
