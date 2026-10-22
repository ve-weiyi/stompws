package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"

	"github.com/ve-weiyi/stompws/logws"
)

// dialRawWebSocket 连一个不做 STOMP 处理的裸 WebSocket 服务端，
// 供需要自己造 Conn 状态的测试使用（避免 hub 在同一条连接上也跑读写循环）。
func dialRawWebSocket(t *testing.T) *websocket.Conn {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial raw websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ──────────────── 阶段 1：升级对齐 ────────────────

func TestAllocateMessageIdSetsAckPerMode(t *testing.T) {
	cases := []struct {
		name        string
		ack         string
		withSub     bool
		wantAck     bool
		wantSubHead bool
	}{
		{"auto 订阅摘掉 ack", frame.AckAuto, true, false, true},
		{"client 订阅回 message-id 作为 ack", frame.AckClient, true, true, true},
		{"client-individual 订阅同理", frame.AckClientIndividual, true, true, true},
		{"无订阅（topic 投递）摘掉 ack", "", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 本用例只钉 allocateMessageId 的头部处理，不触碰中继，故 up 传 nil。
			c := NewConn(Config{}, nil, nil)
			f := frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs", frame.Ack, "leaked")

			var sub *Subscription
			if tc.withSub {
				sub = NewSubscription(c, "/queue/jobs", "sub-1", tc.ack)
			}

			c.allocateMessageId(f, sub)

			ack, hasAck := f.Header.Contains(frame.Ack)
			if hasAck != tc.wantAck {
				t.Errorf("ack 头存在 = %v (%q), want %v", hasAck, ack, tc.wantAck)
			}
			if tc.wantAck && ack != f.Header.Get(frame.MessageId) {
				t.Errorf("ack = %q, want 与 message-id %q 相同", ack, f.Header.Get(frame.MessageId))
			}

			subHead := f.Header.Get(frame.Subscription)
			if got := subHead != ""; got != tc.wantSubHead {
				t.Errorf("subscription = %q, want 存在=%v", subHead, tc.wantSubHead)
			}
			if f.Header.Get(frame.MessageId) == "" {
				t.Error("message-id 为空：MESSAGE 必带 message-id")
			}
		})
	}
}

// ──────────────── 阶段 2：心跳 ────────────────

// 中继循环是全体连接共用的：投递缓冲满时必须断开那个慢消费者，而不是阻塞。
// 这里直接从内部填满缓冲 —— 靠真实 socket 自然写不动要同时填满两端 OS 缓冲，
// 容量随系统调优，测试会不稳。
func TestDeliverDisconnectsSlowConsumer(t *testing.T) {
	b := newFakeHub()

	cases := []struct {
		name    string
		fill    func(c *Conn)
		deliver func(c *Conn)
	}{
		{
			name: "writeChan（topic 投递）",
			fill: func(c *Conn) {
				for i := 0; i < cap(c.writeChannel); i++ {
					c.writeChannel <- frame.New(frame.MESSAGE, frame.Destination, "/topic/x")
				}
			},
			deliver: func(c *Conn) {
				c.deliverFrame(frame.New(frame.MESSAGE, frame.Destination, "/topic/x"))
			},
		},
		{
			name: "subChan（队列投递）",
			fill: func(c *Conn) {
				for i := 0; i < cap(c.subChannel); i++ {
					c.subChannel <- NewSubscription(c, "/queue/q", "sub-x", frame.AckAuto)
				}
			},
			deliver: func(c *Conn) {
				c.deliverSub(NewSubscription(c, "/queue/q", "sub-x", frame.AckAuto))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConn(Config{Log: &logws.NoOpLogger{}}, dialRawWebSocket(t), b)

			tc.fill(c)

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.deliver(c)
			}()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("缓冲满时投递阻塞了：中继会被慢消费者拖住")
			}

			select {
			case <-c.closeChan:
			default:
				t.Error("缓冲满时没有断开该连接")
			}
		})
	}
}

// writeLoop 退出时把 subChan 里未写出的队列帧退回队首 —— 那是它们唯一的归属处。
func TestRequeueUnwrittenFrames(t *testing.T) {
	b := newFakeHub()

	c := NewConn(Config{Log: &logws.NoOpLogger{}}, nil, b)

	subs := []*Subscription{
		NewSubscription(c, "/queue/jobs", "sub-1", frame.AckAuto),
		NewSubscription(c, "/queue/jobs", "sub-2", frame.AckAuto),
	}
	for _, sub := range subs {
		sub.frame = frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs")
		c.subChannel <- sub
	}

	c.requeueUnwritten()

	got := b.tookRequeue()
	if len(got) != len(subs) {
		t.Fatalf("退回 %d 帧, want %d", len(got), len(subs))
	}
	for i, sub := range subs {
		if sub.frame != nil {
			t.Errorf("第 %d 个订阅的 frame 标记没被清掉", i+1)
		}
	}
}

// 投递动作与 writeLoop 的退出必须互斥：否则排空之后落进 subChan 的帧既没人写出、
// 也不在 subList 里（断开清理看不到），而投递还报「成功」因而不退回，帧就消失了。
func TestDeliverAfterWriteLoopGoneIsNotHandedOff(t *testing.T) {
	b := newFakeHub()

	c := NewConn(Config{Log: &logws.NoOpLogger{}}, dialRawWebSocket(t), b)

	// 制造「writeLoop 已经退出」的状态：关掉它（closeChan 关闭）并跑一次排空。
	c.close()
	c.requeueUnwritten()

	// 此后任何投递都必须报告「没交出去」，让调用方退回队首。
	// 这里跑多轮：随机 select 在两路同时就绪时会各选一半，单轮说明不了问题。
	const rounds = 200
	handedOff := 0
	for i := 0; i < rounds; i++ {
		sub := NewSubscription(c, "/queue/jobs", "sub-1", frame.AckAuto)
		sub.frame = frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs")
		if c.deliverSub(sub) {
			handedOff++
		}
	}

	if handedOff != 0 {
		t.Fatalf("writeLoop 已退出，仍有 %d/%d 次投递被报成「成功」—— 这些帧无人写出也无人退回", handedOff, rounds)
	}
}
