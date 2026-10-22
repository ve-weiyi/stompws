package client

import (
	"errors"
	"io"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"

	"github.com/ve-weiyi/stompws/logws"
)

// Maximum number of pending frames allowed to a client.
// before a disconnect occurs. If the client cannot keep
// up with the server, we do not want the server to backlog
// pending frames indefinitely.
const maxPendingWrites = 256

// Maximum number of pending frames allowed before the read
// go routine starts blocking.
const maxPendingReads = 16

// Conn 是一条已建立的客户端连接（一连接 = 一个 STOMP 会话），也是回调拿到的句柄。
type Conn struct {
	config            Config // 服务端注入的连接级参数
	ws                *websocket.Conn
	up                Hub // 本连接向上调用的中继句柄
	subChannel        chan *Subscription
	writeChannel      chan *frame.Frame
	readChannel       chan *frame.Frame
	connected         bool // 是否已完成 CONNECT
	writeTimeoutNanos atomic.Int64
	version           stomp.Version
	txStore           *txStore
	lastMsgId         uint64
	subList           *SubscriptionList
	subscriptions     map[string]*Subscription
	validator         stomp.Validator
	log               logws.Logger

	// ── 以下为本仓独有 ──

	// 会话身份：id/login/version 只由 processLoop 读写，readLoop、writeLoop 只用对端地址记日志。
	id    string // client id. It is device id or ip address.
	login string // client login. It is username or userId.

	// 三条协程与投递的生命周期。
	closeChan chan struct{}
	closeOnce sync.Once
	// deliveryMu 与 writeLoopGone 让「投递」与「writeLoop 退出」互斥 —— 否则排空之后
	// 落进 subChannel 的帧没人写出、也不在 subList 里，而投递报成功，帧就丢了。
	deliveryMu    sync.Mutex
	writeLoopGone bool
	// subListMu 护 subList（非并发安全）：写出拆到 writeLoop 后，Add 与 Ack/Nack 分属两个协程。
	subListMu sync.Mutex
}

// NewConn 建一条连接（不启动，由 Serve 启动）。config 与 up 都由服务端注入；
// config 里没填的字段由 withDefaults 兜底 —— 漏配不该表现为「第一次打日志就
// nil 解引用」或「读 deadline 立即过期」。
func NewConn(config Config, ws *websocket.Conn, up Hub) *Conn {
	config = config.withDefaults()

	return &Conn{
		config:        config,
		ws:            ws,
		up:            up,
		log:           config.Log,
		subChannel:    make(chan *Subscription, maxPendingWrites),
		writeChannel:  make(chan *frame.Frame, maxPendingWrites),
		readChannel:   make(chan *frame.Frame, maxPendingReads),
		subscriptions: make(map[string]*Subscription),
		subList:       NewSubscriptionList(),
		closeChan:     make(chan struct{}),
		txStore:       newTxStore(),
	}
}

func (c *Conn) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			c.log.Errorf("peer=%s: readLoop panic: %v\nstack: %s", c.GetIpAddress(), r, debug.Stack())
		}
		c.close()
	}()

	// 读超时由本协程从 CONNECT 帧算（本协程先于 CONNECT 启动，放字段上就要同步）。
	expectingConnect := true
	var readTimeout time.Duration

	// 一个 frame.Reader 贯穿整条连接；wsFrameReader 负责把 WS 消息按帧喂给它，并守住帧的上限。
	reader := frame.NewReader(newWSFrameReader(c.ws, maxFrameBody, maxHeaderBytes))

	for {
		switch {
		case readTimeout > 0:
			if err := c.ws.SetReadDeadline(time.Now().Add(readTimeout * 2)); err != nil {
				c.log.Errorf("peer=%s: set read deadline failed: %v", c.GetIpAddress(), err)
			}
		case expectingConnect:
			// CONNECT 之前也要有时限，否则空连接能靠协程与通道缓冲永久占资源。
			if err := c.ws.SetReadDeadline(time.Now().Add(c.config.ConnectTimeout)); err != nil {
				c.log.Errorf("peer=%s: set read deadline failed: %v", c.GetIpAddress(), err)
			}
		default:
			// 没协商出心跳（cx=0，含缺头的 0,0）就不做读超时判定，但必须清掉 CONNECT
			// 阶段设下的那条 —— 留着它，空闲连接会在 connectTimeout 之后被误杀。
			if err := c.ws.SetReadDeadline(time.Time{}); err != nil {
				c.log.Errorf("peer=%s: clear read deadline failed: %v", c.GetIpAddress(), err)
			}
		}

		f, err := reader.Read()
		if err != nil {
			// 只有「帧畸形」才回 ERROR：流式读无法重新对齐，回完即断开由客户端重连。
			if errors.Is(err, frame.ErrInvalidCommand) || errors.Is(err, frame.ErrInvalidFrameFormat) {
				c.log.Warningf("peer=%s: frame parse error: %v", c.GetIpAddress(), err)
				c.SendErrorAndClose(errFrameParse(err), "")

				// 给 writeLoop 一点时间把 ERROR 发出去，否则它随关连接一起丢掉。
				select {
				case <-time.After(100 * time.Millisecond):
				case <-c.closeChan:
				}
				return
			}

			// 超过读上限（单条 WS 消息、单条帧各有一道）按规范报错并关闭。
			if errors.Is(err, websocket.ErrReadLimit) || errors.Is(err, errFrameTooLarge) {
				c.SendErrorAndClose(errFrameTooLarge, "")
				return
			}

			// 其余是底层读失败：对端正常关闭（EOF）不算错误，读超时等按 error 记。
			if errors.Is(err, io.EOF) {
				c.log.Infof("peer=%s: closed by peer", c.GetIpAddress())
			} else {
				c.log.Errorf("peer=%s: read failed: %v", c.GetIpAddress(), err)
			}
			return
		}
		if f == nil {
			continue // 心跳
		}

		// 与 handleConnect 各算一份是有意的：两个协程就不必共享这个值。
		if expectingConnect && (f.Command == frame.CONNECT || f.Command == frame.STOMP) {
			// 解析失败按 0 处理、不在这里报错：报告交给 handleConnect。
			cx, _, _ := parseHeartBeat(f.Header.Get(frame.HeartBeat))
			if cx > 0 && cx < c.config.MinHeartBeat {
				cx = c.config.MinHeartBeat
			}
			readTimeout = cx
			expectingConnect = false
		}

		select {
		case c.readChannel <- f:
		case <-c.closeChan:
			return
		}
	}
}

func (c *Conn) writeLoop() {
	defer func() {
		// 退出前退回 subChan 里还没写出的队列帧 —— 本协程是它的唯一消费者。
		c.requeueUnwritten()

		if r := recover(); r != nil {
			c.log.Errorf("peer=%s: writeLoop panic: %v\nstack: %s", c.GetIpAddress(), r, debug.Stack())
		}
		c.close()
	}()

	// ticker 只能惰性创建：本协程先于 CONNECT 启动，那时写超时还是 0。
	var ticker *time.Ticker
	var tickerChan <-chan time.Time
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()

	for {
		if ticker == nil {
			if d := c.writeTimeout(); d > 0 {
				ticker = time.NewTicker(d)
				tickerChan = ticker.C
			}
		}

		select {
		case f := <-c.writeChannel:
			// 写出前给 MESSAGE 分配 message-id（topic 走这条路）。
			c.allocateMessageId(f, nil)
			if err := c.sendImmediately(f); err != nil {
				c.log.Errorf("peer=%s: send failed: %v", c.GetIpAddress(), err)
				c.close()
				return
			}
		case sub := <-c.subChannel:
			// 写出前检查订阅是否仍在：已退订就退回而不写出。
			if !sub.active.Load() {
				c.requeuePending(sub)
				continue
			}

			c.allocateMessageId(sub.frame, sub)
			if err := c.sendImmediately(sub.frame); err != nil {
				// 这一帧没送出去，退回队首再断。
				c.requeuePending(sub)
				c.log.Errorf("peer=%s: send failed: %v", c.GetIpAddress(), err)
				c.close()
				return
			}
			if sub.ack == frame.AckAuto {
				sub.frame = nil
				// 队列订阅投递完要挂回队列池。这条路径只有队列帧会走（topic 走 deliverFrame），
				// 故不必判目的地；已退订则不挂回，否则死订阅会被挂回队列池。
				if sub.active.Load() {
					if err := c.up.Attach(sub); err != nil {
						c.log.Errorf("peer=%s: resubscribe %s failed: %v", c.GetIpAddress(), sub.dest, err)
					}
				}
			} else {
				c.subListMu.Lock()
				c.subList.Add(sub)
				c.subListMu.Unlock()
			}
		case <-tickerChan:
			// 发 STOMP 心跳（"\n" 文本帧），不是 WebSocket ping：主流客户端只用数据帧
			// 刷新「服务端活跃时间」，只发 ping 会让空闲连接被它们判成心跳丢失并断开。
			if err := c.ws.WriteMessage(websocket.TextMessage, []byte("\n")); err != nil {
				c.close()
				return
			}
		case <-c.closeChan:
			return
		}
	}
}

func (c *Conn) allocateMessageId(f *frame.Frame, sub *Subscription) {
	if f.Command != frame.MESSAGE {
		return
	}

	c.lastMsgId++
	msgId := strconv.FormatUint(c.lastMsgId, 10)
	f.Header.Set(frame.MessageId, msgId)

	if sub != nil {
		sub.msgId = c.lastMsgId
		f.Header.Set(frame.Subscription, sub.id)
	}

	// ack 头按订阅的 ack 模式定：auto 与无订阅（topic 投递）都要摘掉 —— 留着它等于
	// 告诉一个 auto 订阅「要确认」。帧可能来自应用自建，不能假定它没带 ack 头。
	if sub == nil || sub.ack == frame.AckAuto {
		f.Header.Del(frame.Ack)
	} else {
		f.Header.Set(frame.Ack, msgId)
	}
}

// sendImmediately 编码并写出一个帧，不经过 writeChannel 排队。错误交给调用方处置。
func (c *Conn) sendImmediately(f *frame.Frame) error {
	data, err := frameToBytes(f)
	if err != nil {
		return err
	}
	err = c.ws.WriteMessage(websocket.TextMessage, data)
	return err
}

func (c *Conn) SendFrame(f *frame.Frame) {
	select {
	case c.writeChannel <- f:
	case <-time.After(time.Second):
		c.close()
	case <-c.closeChan:
	}
}

// deliverSub 把订阅交给 writeLoop，不阻塞中继循环。返回 false 表示没有交出去，
// 调用方必须把这帧退回队首。缓冲满即判慢消费者并断开该连接。
func (c *Conn) deliverSub(sub *Subscription) bool {
	c.deliveryMu.Lock()
	defer c.deliveryMu.Unlock()

	if c.writeLoopGone {
		return false
	}

	select {
	case c.subChannel <- sub:
		return true
	case <-c.closeChan:
		// 连接已关，投递没有意义；这不是慢消费者，不记警告。
		return false
	default:
		c.log.Warningf("peer=%s: slow consumer, dropping connection", c.GetIpAddress())
		c.close()
		return false
	}
}

// deliverFrame 把帧交给 writeLoop，不阻塞中继循环。语义同 deliverSub；
// topic 是 fire-and-forget，投不出去没有「退回队首」可言。
func (c *Conn) deliverFrame(f *frame.Frame) {
	c.deliveryMu.Lock()
	defer c.deliveryMu.Unlock()

	if c.writeLoopGone {
		return
	}

	select {
	case c.writeChannel <- f:
	case <-c.closeChan:
	default:
		c.log.Warningf("peer=%s: slow consumer, dropping connection", c.GetIpAddress())
		c.close()
	}
}

// requeueUnwritten 把 subChannel 里没写出的队列帧退回队首，只由 writeLoop 退出时调用。
// 置位与抽取在 deliveryMu 下，退回放锁外（Requeue 在请求通道满时会阻塞）。
func (c *Conn) requeueUnwritten() {
	c.deliveryMu.Lock()
	c.writeLoopGone = true

	var pending []*Subscription
	for {
		select {
		case sub := <-c.subChannel:
			pending = append(pending, sub)
		default:
			c.deliveryMu.Unlock()
			for _, sub := range pending {
				c.requeuePending(sub)
			}
			return
		}
	}
}

// requeuePending 把订阅上挂着的、取出来但没能交付的帧退回队首。
// 是不是队列帧由中继判（topic 是 fire-and-forget，实现侧直接忽略）。
func (c *Conn) requeuePending(sub *Subscription) {
	f := sub.frame
	if f == nil {
		return
	}
	sub.frame = nil

	c.up.Requeue(f)
}

// GetSessionInfo 返回会话身份（id、login、协商版本）；只能在回调（同一协程）里调用。
func (c *Conn) GetSessionInfo() (id, login string, version stomp.Version) {
	return c.id, c.login, c.version
}

func (c *Conn) GetIpAddress() string {
	if c.ws != nil {
		return c.ws.RemoteAddr().String()
	}
	return ""
}

// setWriteTimeout 记下协商出的心跳写超时（只由 processLoop 调）。
func (c *Conn) setWriteTimeout(d time.Duration) {
	c.writeTimeoutNanos.Store(int64(d))
}

// writeTimeout 取出协商出的心跳写超时（0 = 不发心跳；只由 writeLoop 读）。
func (c *Conn) writeTimeout() time.Duration {
	return time.Duration(c.writeTimeoutNanos.Load())
}

// Serve 启动三条协程（读/写/处理），全部退出后返回 —— 服务端据此等待关停。
func (c *Conn) Serve() {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); c.readLoop() }()
	go func() { defer wg.Done(); c.writeLoop() }()
	go func() { defer wg.Done(); c.processLoop() }()
	wg.Wait()
}

func (c *Conn) close() {
	c.closeOnce.Do(func() {
		close(c.closeChan)
		// 关连接失败没有可补救的动作，忽略。
		_ = c.ws.Close()
	})
}
