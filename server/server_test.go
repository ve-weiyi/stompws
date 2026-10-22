// StompHubServer 自身的端到端行为：连接与生命周期、关停、路由与投递、报文边界。
//
// 这些用例驱动的是组合起来的 StompHubServer（server.go），故都落在本文件。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"
)

// 协商出读超时后，客户端一直不发数据，服务端必须主动断开 ——
// 否则客户端掉线（没有 TCP FIN）时连接会一直挂在服务器上。
func TestServerClosesIdleClientAfterReadTimeout(t *testing.T) {
	s := newTestHub(t, WithMinHeartBeat(testHeartBeat))
	conn := dialTestClient(t, startTestServer(t, s))

	// cx=50 表示客户端承诺每 50ms 发一次心跳，cy=0 表示它不想收到心跳。
	connect(t, conn, "50,0")

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("ReadMessage() = nil error, want the server to close the idle connection")
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("客户端是自己读超时的：服务端并未在读超时到点后断开连接")
	}
}

// 客户端不承诺发心跳（缺 heart-beat 头，按规范等同 0,0；或 cx=0）时，服务端不能对它
// 做读超时判定 —— 它没做出过任何承诺，否则会因「没按自己从未做出的承诺发心跳」被误杀。
// CONNECT 阶段设下的那条读超时也必须一并清掉，否则空闲连接照样会在 connectTimeout 后断。
func TestServerKeepsIdleClientWithoutHeartBeat(t *testing.T) {

	cases := []struct{ name, heartBeat string }{
		{"缺 heart-beat 头（规范：等同 0,0）", ""},
		{"cx=0、只想收心跳", "0,50"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestHub(t, WithMinHeartBeat(testHeartBeat), WithConnectTimeout(testConnectTimeout))
			conn := dialTestClient(t, startTestServer(t, s))

			connect(t, conn, tc.heartBeat)

			// 全程不发任何数据，等过 CONNECT 阶段的读超时，连接仍须可用。
			time.Sleep(3 * testConnectTimeout)

			writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
				"destination": "/topic/room", "id": "sub-1", "receipt": "r-1",
			})

			// 服务端心跳是单个 "\n"，解析出来命令为空，跳过它。
			for {
				command, _, _ := readSTOMPFrame(t, conn)
				if command == frame.RECEIPT {
					return
				}
				if command != "" {
					t.Fatalf("读到 %q, want RECEIPT", command)
				}
			}
		})
	}
}

// SEND 的 receipt 头不能随路由出去的 MESSAGE 泄漏给订阅者：回执是发送方与服务器之间的
// 约定，订阅者看到它会以为这条消息要自己确认（上游 sendReceiptImmediately 同样在回执时摘掉）。
func TestRouteMessageDoesNotLeakReceiptHeader(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&passthroughHook{})))

	subscriber := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	writeSTOMPFrame(t, sender, frame.SEND, map[string]string{
		"destination": "/topic/room", "receipt": "r-1",
	})

	expectFrame(t, sender, frame.RECEIPT)

	headers := expectFrame(t, subscriber, frame.MESSAGE)
	if got := headers[frame.Receipt]; got != "" {
		t.Errorf("投递给订阅者的 MESSAGE 带上了发送方的 receipt=%q", got)
	}
}

// ──────────────── 投递 ────────────────

// SUBSCRIBE 带 receipt 时，服务端必须等订阅真的挂上之后再回 RECEIPT
// （SubscribeOpt.Receipt 消除丢消息竞态靠的就是这条语义），且 MESSAGE 头要齐。
func TestSendReachesSubscriber(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	subscriber := dialTestClient(t, url)
	connect(t, subscriber, "")
	writeSTOMPFrame(t, subscriber, frame.SUBSCRIBE, map[string]string{
		"destination": "/topic/room",
		"id":          "sub-1",
		"receipt":     "receipt-1",
	})
	expectFrame(t, subscriber, frame.RECEIPT)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	writeSTOMPFrame(t, sender, frame.SEND, map[string]string{"destination": "/topic/room"})

	headers := expectFrame(t, subscriber, frame.MESSAGE)
	if got := headers[frame.Destination]; got != "/topic/room" {
		t.Errorf("destination = %q, want /topic/room", got)
	}
	if got := headers[frame.Subscription]; got != "sub-1" {
		t.Errorf("subscription = %q, want sub-1", got)
	}
	// STOMP 1.2 要求 MESSAGE 带 message-id；上游也正是在写出前调
	// allocateMessageId(f, nil) 补上它的。
	if got := headers[frame.MessageId]; got == "" {
		t.Error("MESSAGE 缺 message-id")
	}
}

// 多个连接同时走完 CONNECT → SUBSCRIBE（带回执）→ SEND → 收 MESSAGE → DISCONNECT。
// 守两条防线：连接建立前后的跨协程写入点；各客户端订阅不同目的地时 Manager.Find 的并发写 map。
func TestConcurrentClients(t *testing.T) {
	const clientCount = 8

	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{}), WithMinHeartBeat(testHeartBeat)))

	errs := make(chan error, clientCount)
	var wg sync.WaitGroup

	for i := 0; i < clientCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- runClientOnce(url, i)
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// runClientOnce 走完一个客户端的完整生命周期，异常一律返回 error。
// 心跳取 "0,50"：cx=0 让服务端不给本连接设读 deadline（并发下 100ms 读超时不稳），
// cy=50 仍能覆盖 writeLoop 跨协程读 writeTimeout 的路径。
func runClientOnce(url string, i int) error {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("client %d: dial: %w", i, err)
	}
	defer conn.Close()

	if err := writeRaw(conn, frame.CONNECT, map[string]string{
		"accept-version": "1.2",
		"host":           "/",
		"heart-beat":     "0,50",
	}); err != nil {
		return fmt.Errorf("client %d: write CONNECT: %w", i, err)
	}
	if err := expectRaw(conn, i, frame.CONNECTED); err != nil {
		return err
	}

	// 目的地各不相同 —— 这样才能真正并发写 Manager 的 map。
	dest := fmt.Sprintf("/topic/room-%d", i)
	if err := writeRaw(conn, frame.SUBSCRIBE, map[string]string{
		"destination": dest,
		"id":          "sub-1",
		"receipt":     "receipt-1",
	}); err != nil {
		return fmt.Errorf("client %d: write SUBSCRIBE: %w", i, err)
	}
	if err := expectRaw(conn, i, frame.RECEIPT); err != nil {
		return err
	}

	if err := writeRaw(conn, frame.SEND, map[string]string{"destination": dest}); err != nil {
		return fmt.Errorf("client %d: write SEND: %w", i, err)
	}
	if err := expectRaw(conn, i, frame.MESSAGE); err != nil {
		return err
	}

	if err := writeRaw(conn, frame.DISCONNECT, nil); err != nil {
		return fmt.Errorf("client %d: write DISCONNECT: %w", i, err)
	}
	return nil
}

// expectRaw 读到下一个帧并断言命令，失败时把客户端序号带进错误。
func expectRaw(conn *websocket.Conn, i int, wantCommand string) error {
	command, headers, _, err := readFrameRaw(conn)
	if err != nil {
		return fmt.Errorf("client %d: read %s: %w", i, wantCommand, err)
	}
	if command != wantCommand {
		return fmt.Errorf("client %d: got %q (headers %v), want %q", i, command, headers, wantCommand)
	}
	return nil
}

// ──────────────── 阶段 4：中继串行化 ────────────────

// 关停之后仍然提交必须优雅失败：既不能 panic（send on closed channel），
// 也不能永久阻塞 —— 外部协程（如 blog-cloud 的在线人数广播）并不知道服务端已停。
func TestRouteMessageAfterShutdown(t *testing.T) {
	s := newTestHub(t)

	conn := dialTestClient(t, startTestServer(t, s))
	connect(t, conn, "")

	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown 允许多次调用（调用方在错误分支上再关一次很常见），必须幂等。
	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("第二次 Shutdown: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.RouteMessage(nil, frame.New(frame.MESSAGE, frame.Destination, "/topic/room"))
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("关停后 RouteMessage 阻塞了")
	}
}

// 队列 + ack 模式：投递写 sub.frame、ACK 清它，两条路径分属不同协程，不能竞争；
// 同时覆盖 1.1 用 message-id、1.2 用 id 两种 ACK 头形状（ACK 被忽略则该订阅不再入队，
// 测试会在第二条消息上超时）。
func TestQueueAckClearsPendingFrame(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		ackHeader string
	}{
		{"1.1 用 message-id", "1.1", frame.MessageId},
		{"1.2 用 id", "1.2", frame.Id},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestHub(t, WithEventHooks(&routeHook{}))
			url := startTestServer(t, s)

			subscriber := dialTestClient(t, url)
			writeSTOMPFrame(t, subscriber, frame.CONNECT, map[string]string{
				"accept-version": tc.version,
				"host":           "/",
			})
			expectFrame(t, subscriber, frame.CONNECTED)

			writeSTOMPFrame(t, subscriber, frame.SUBSCRIBE, map[string]string{
				"destination": "/queue/jobs",
				"id":          "sub-1",
				"ack":         frame.AckClient,
				"receipt":     "receipt-1",
			})
			expectFrame(t, subscriber, frame.RECEIPT)

			sender := dialTestClient(t, url)
			connect(t, sender, "")

			const messages = 20
			for i := 0; i < messages; i++ {
				writeSTOMPFrame(t, sender, frame.SEND, map[string]string{"destination": "/queue/jobs"})
			}

			// 每收到一条就 ACK：ACK 走 processLoop，投递走中继 goroutine。
			for i := 0; i < messages; i++ {
				headers := expectFrame(t, subscriber, frame.MESSAGE)
				msgID := headers[frame.MessageId]
				if msgID == "" {
					t.Fatalf("第 %d 条 MESSAGE 缺 message-id", i+1)
				}
				writeSTOMPFrame(t, subscriber, frame.ACK, map[string]string{tc.ackHeader: msgID})
			}
		})
	}
}

// ──────────────── 阶段 5：在途帧的归属 ────────────────

// 队列订阅者被硬断开、或先退订再断开时，已交付但未确认的帧必须退回队首
// （第二个订阅者要能拿到它）。
func TestQueueUnackedFrameSurvivesDisconnect(t *testing.T) {
	cases := []struct {
		name        string
		unsubscribe bool
	}{
		{"直接断开", false},
		{"先退订再断开", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestHub(t, WithEventHooks(&routeHook{}))
			url := startTestServer(t, s)

			first := dialTestClient(t, url)
			writeSTOMPFrame(t, first, frame.CONNECT, map[string]string{
				"accept-version": "1.1",
				"host":           "/",
			})
			expectFrame(t, first, frame.CONNECTED)
			writeSTOMPFrame(t, first, frame.SUBSCRIBE, map[string]string{
				"destination": "/queue/jobs",
				"id":          "sub-1",
				"ack":         frame.AckClient,
				"receipt":     "receipt-1",
			})
			expectFrame(t, first, frame.RECEIPT)

			sender := dialTestClient(t, url)
			connect(t, sender, "")
			writeSTOMPFrame(t, sender, frame.SEND, map[string]string{"destination": "/queue/jobs"})

			// 拿到但故意不 ACK —— 这一帧就成了「已交付未确认」。
			expectFrame(t, first, frame.MESSAGE)

			if tc.unsubscribe {
				writeSTOMPFrame(t, first, frame.UNSUBSCRIBE, map[string]string{"id": "sub-1"})
			}
			_ = first.Close()

			second := dialTestClient(t, url)
			writeSTOMPFrame(t, second, frame.CONNECT, map[string]string{
				"accept-version": "1.1",
				"host":           "/",
			})
			expectFrame(t, second, frame.CONNECTED)
			writeSTOMPFrame(t, second, frame.SUBSCRIBE, map[string]string{
				"destination": "/queue/jobs",
				"id":          "sub-2",
				"ack":         frame.AckClient,
				"receipt":     "receipt-2",
			})
			// 订阅非空队列时服务端会先投 MESSAGE 再回 RECEIPT，顺序不定；
			// MESSAGE 到来说明那一帧确实被退回队列了。
			expectBoth(t, second, 2, frame.MESSAGE, frame.RECEIPT)
		})
	}
}

// ──────────────── 投递与连接生命周期 ────────────────

// 退订之后，在途的 ACK 不得把该订阅复活回队列：死订阅会替活订阅吃掉队列消息。
func TestUnsubscribedSubscriptionIsNotRevived(t *testing.T) {
	s := newTestHub(t, WithEventHooks(&routeHook{}))
	url := startTestServer(t, s)

	gone := dialTestClient(t, url)
	writeSTOMPFrame(t, gone, frame.CONNECT, map[string]string{"accept-version": "1.1", "host": "/"})
	expectFrame(t, gone, frame.CONNECTED)
	writeSTOMPFrame(t, gone, frame.SUBSCRIBE, map[string]string{
		"destination": "/queue/jobs",
		"id":          "sub-1",
		"ack":         frame.AckClient,
		"receipt":     "receipt-1",
	})
	expectBoth(t, gone, 1, frame.RECEIPT)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	writeSTOMPFrame(t, sender, frame.SEND, map[string]string{"destination": "/queue/jobs"})
	headers := expectFrame(t, gone, frame.MESSAGE)

	// 退订，然后为刚收到的那条补 ACK —— 两者都是合规的客户端行为。
	writeSTOMPFrame(t, gone, frame.UNSUBSCRIBE, map[string]string{"id": "sub-1"})
	writeSTOMPFrame(t, gone, frame.ACK, map[string]string{"message-id": headers[frame.MessageId]})
	time.Sleep(50 * time.Millisecond) // 等 ACK 的重新订阅（若有）走完

	// 再来一个订阅者，然后发第二条：它必须交给活着的那一个。
	alive := dialTestClient(t, url)
	writeSTOMPFrame(t, alive, frame.CONNECT, map[string]string{"accept-version": "1.1", "host": "/"})
	expectFrame(t, alive, frame.CONNECTED)
	writeSTOMPFrame(t, alive, frame.SUBSCRIBE, map[string]string{
		"destination": "/queue/jobs",
		"id":          "sub-2",
		"ack":         frame.AckClient,
		"receipt":     "receipt-2",
	})

	writeSTOMPFrame(t, sender, frame.SEND, map[string]string{"destination": "/queue/jobs"})
	expectBoth(t, alive, 2, frame.MESSAGE, frame.RECEIPT)

	// 已退订的那条连接不得再收到任何消息。
	if err := gone.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, data, err := gone.ReadMessage(); err == nil {
		t.Errorf("已退订的连接又收到了 %q —— 订阅被复活了", data)
	}
}

// CONNECTED 里承诺的 STOMP 心跳必须真的以数据帧（"\n"）发出：主流客户端只用数据帧
// 刷新「服务端活跃时间」，只发 WebSocket ping 会让它们判成心跳丢失并断开空闲连接。
func TestServerSendsStompHeartBeats(t *testing.T) {
	s := newTestHub(t, WithMinHeartBeat(testHeartBeat))
	conn := dialTestClient(t, startTestServer(t, s))

	// cx=0：客户端不发心跳；cy=50：它希望每 50ms 收到一次心跳。
	connect(t, conn, "0,50")

	const window = 300 * time.Millisecond
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}

	beats := 0
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if string(data) == "\n" {
			beats++
		}
	}

	if beats < 2 {
		t.Fatalf("%v 内只收到 %d 次 STOMP 心跳（间隔 50ms），期望至少 2 次", window, beats)
	}
}

// ──────────────── 关停、回执与契约 ────────────────

// 中继卡在某个请求里（慢 storage / 慢 logger）+ 请求通道被灌满时，Shutdown 也必须
// 按超时返回。否则 k8s 停 Pod 会一直等到 grace period 被 SIGKILL。
func TestShutdownReturnsWhenBrokerStuck(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)

	s := newTestHub(t, WithQueueStorage(&fakeStorage{gate: gate}))

	// 第 1 条把中继钉在 storage 上；后续灌满请求通道；再往后的提交者会握着
	// 「提交锁」阻塞在通道上 —— 这正是让旧版 stop() 永久等待的条件。
	for i := 0; i < 200; i++ {
		go func() {
			_ = s.proc.submit(Request{
				Op:    RequeueOp,
				Frame: frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs"),
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- s.Shutdown(time.Second) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("中继卡住时 Shutdown 应当以错误返回（超时），而不是报成功")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("中继卡住时 Shutdown 没有返回 —— 超时承诺失效")
	}
}

// 事务相关的帧：回执不能早于校验（否则先谎报成功、再回 ERROR，消息还丢了），
// 重放也不能把事务内 SEND 的回执再发一次。
func TestTransactionReceiptTiming(t *testing.T) {
	s := newTestHub(t, WithEventHooks(&routeHook{}))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	// 注：未知事务那条现在是致命的（ERROR + 关闭），这里只留回执时序。

	t.Run("事务内 SEND 的回执只发一次", func(t *testing.T) {
		writeSTOMPFrame(t, conn, frame.BEGIN, map[string]string{"transaction": "tx1"})
		writeSTOMPFrame(t, conn, frame.SEND, map[string]string{
			"destination": "/topic/room", "transaction": "tx1", "receipt": "r-1",
		})
		expectFrame(t, conn, frame.RECEIPT)

		writeSTOMPFrame(t, conn, frame.COMMIT, map[string]string{
			"transaction": "tx1", "receipt": "r-2",
		})
		expectFrame(t, conn, frame.RECEIPT)

		// COMMIT 之后不该再冒出第二个 RECEIPT（重放又回了一次 r-1）。
		if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		if _, data, err := conn.ReadMessage(); err == nil {
			t.Errorf("COMMIT 之后又收到 %q —— 事务内帧的回执被重放重复发送了", data)
		}
	})
}

// 被 hook 拒绝的 SEND 不能回 RECEIPT：那等于把「拒绝」报成「已受理」。
func TestSendRejectedByHookHasNoReceipt(t *testing.T) {
	s := newTestHub(t, WithEventHooks(&rejectHook{}))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")
	writeSTOMPFrame(t, conn, frame.SEND, map[string]string{
		"destination": "/topic/room", "receipt": "r-1",
	})

	if got := readUntilQuiet(t, conn, 400*time.Millisecond); len(got) != 0 {
		t.Errorf("被 hook 拒绝的 SEND 却收到了 %v", got)
	}
}

// ──────────────── 报文边界 ────────────────

// CONNECT 帧本身也要过验证器。validator 过去在 handleConnect 末尾才装，
// 而帧校验发生在协商出版本之前（那时 validator 还是 nil）—— 首帧从来没被校验过。
func TestConnectFrameIsValidated(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		// 注：缺 host 不在此列（另有专门用例钉住它）。
		{"心跳格式非法", map[string]string{"accept-version": "1.2", "host": "/", "heart-beat": "abc"}},
		{"CONNECT 带 receipt", map[string]string{"accept-version": "1.2", "host": "/", "receipt": "r1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestHub(t)
			conn := dialTestClient(t, startTestServer(t, s))

			writeSTOMPFrame(t, conn, frame.CONNECT, tc.headers)
			expectFrame(t, conn, frame.ERROR)
		})
	}
}

// Shutdown 必须把在线状态的存储一并关掉，并停掉在线续期循环 ——
// 否则每建/销一个 hub 泄漏一个协程，且会在关停后继续往已关闭的实现上回调。
func TestShutdownStopsPresence(t *testing.T) {
	presence := &countingPresence{}
	s := newTestHub(t, WithPresenceManager(presence), WithOnlineSyncInterval(5*time.Millisecond))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	if err := s.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if !presence.isClosed() {
		t.Error("Shutdown 没有关闭 PresenceManager")
	}

	before := presence.renewCount()
	time.Sleep(50 * time.Millisecond)
	if got := presence.renewCount(); got != before {
		t.Errorf("关停后 presenceSyncLoop 仍在续期：%d → %d", before, got)
	}
}

// 在线状态按**连接**计：同一 login 的两条连接（clientId 不同），断开其一后用户仍在名单里。
// 现状按 login 记账 —— 第二条覆盖第一条、任一断开即把整条登录摘掉，于是还活着的那条连接
// 被判为离线，且不再被续期循环看到（它遍历的就是那张被摘掉的表）。
func TestDisconnectOneOfTwoConnectionsKeepsUserOnline(t *testing.T) {
	hook := &recordingHook{}
	s := newTestHub(t, WithEventHooks(hook))
	url := startTestServer(t, s)

	// 两条连接同一个 login（connectLikeStompjs 固定发 login: uid-1），对端端口不同 → clientId 不同。
	first := dialTestClient(t, url)
	connectLikeStompjs(t, first)
	second := dialTestClient(t, url)
	connectLikeStompjs(t, second)

	// 两条都登记完成再动手：CONNECTED 帧发在登记之前（handlers.go:173 → 178），只看
	// CONNECTED 就断开，会让「第二条还没登记」的时序偶尔蒙对。钩子在登记（含上报 tracker）
	// 之后触发（hub.go:96 → 98），故等钩子计数即等到了登记。
	waitFor(t, func() int { return hook.count(&hook.connects) }, 2, "OnConnect 回调次数")

	if err := first.Close(); err != nil {
		t.Fatalf("close one of two connections: %v", err)
	}
	// 断开路径同样是先上报 tracker、再触发钩子（hub.go:109 → 111）。
	waitFor(t, func() int { return hook.count(&hook.disconnects) }, 1, "OnDisconnect 回调次数")

	stats, err := s.Presence().PresenceStats(context.Background())
	if err != nil {
		t.Fatalf("PresenceStats: %v", err)
	}
	if stats.NumUsers != 1 {
		t.Errorf("同一 login 两条连接断开其一后的在线用户数 = %d, want 1", stats.NumUsers)
	}
	if stats.NumClients != 1 {
		t.Errorf("同一 login 两条连接断开其一后的在线连接数 = %d, want 1", stats.NumClients)
	}

	users, err := s.Presence().Presence(context.Background())
	if err != nil {
		t.Fatalf("Presence: %v", err)
	}
	logins := make([]string, 0, len(users))
	for _, u := range users {
		logins = append(logins, u.Login)
	}
	found := false
	for _, login := range logins {
		if login == "uid-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("在线名单 = %v, want 含 uid-1（它还有一条连接活着）", logins)
	}
}

// RouteMessage 收到的若不是 MESSAGE 帧（最典型：把 handleSend 递来的 SEND 帧直接转进来），
// 必须纠成 MESSAGE —— 否则订阅者收到命令为 SEND 的帧，真实客户端判协议错、测试页静默丢弃。
func TestRouteMessageCoercesCommand(t *testing.T) {
	s := newTestHub(t, WithEventHooks(&routeHook{}))
	url := startTestServer(t, s)

	subscriber := dialTestClient(t, url)
	connect(t, subscriber, "")
	writeSTOMPFrame(t, subscriber, frame.SUBSCRIBE, map[string]string{
		"destination": "/topic/room", "id": "sub-1", "receipt": "r1",
	})
	expectFrame(t, subscriber, frame.RECEIPT)

	s.RouteMessage(nil, frame.New(frame.SEND, frame.Destination, "/topic/room", frame.MessageId, "1"))

	// 能按 MESSAGE 读到，就说明命令被纠正了。
	expectFrame(t, subscriber, frame.MESSAGE)
}

// 未认证客户端也能建立 WebSocket，因此必须有连接数上限兜住资源占用，
// 而且计数要能回落（断开之后又能连上）。
func TestMaxConnectionsIsEnforced(t *testing.T) {
	s := newTestHub(t, WithMaxConnections(2))
	url := startTestServer(t, s)

	c1 := dialTestClient(t, url)
	connect(t, c1, "")
	c2 := dialTestClient(t, url)
	connect(t, c2, "")

	if conn, _, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		_ = conn.Close()
		t.Fatal("超过上限的连接被接受了")
	}

	// 断开一条之后计数应当回落。
	_ = c1.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err == nil {
			t.Cleanup(func() { _ = conn.Close() })
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("断开一条后仍连不上（计数没有回落）：%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 连上但一直不发 CONNECT 的连接必须在 connectTimeout 内被断开 ——
// 没有这道时限，它就能永远占着 3 条协程与通道缓冲。
func TestConnectTimeoutClosesSilentConnection(t *testing.T) {
	s := newTestHub(t, WithConnectTimeout(50*time.Millisecond))
	conn := dialTestClient(t, startTestServer(t, s))

	// 不发送任何帧，等超时：应当读到服务端关闭，而不是自己等超时。
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("服务端没有断开这条不发 CONNECT 的连接")
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("服务端没有在 connectTimeout 内断开：客户端只能等到自己的读超时")
	}
}

// ──────────────── 帧边界：与 WebSocket 消息边界无关 ────────────────

// 声明超大 content-length 的 SEND 必须在**正文分配之前**被拦下（否则 40 字节的帧就能让
// 服务端申请 2GB）。本用例只发头、不发正文 —— 若实现退化成「读正文时才发现超限」，它会超时。
func TestOversizedDeclaredBodyRejectedBeforeReading(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	if err := conn.WriteMessage(websocket.TextMessage,
		[]byte("SEND\ndestination:/topic/room\ncontent-length:2000000000\n\n")); err != nil {
		t.Fatalf("write oversized header: %v", err)
	}

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 一个 WebSocket 消息里可以含多个 STOMP 帧（客户端合并发送）。
// 每消息只解析一帧的话，第二个帧会被静默丢弃。
func TestMultipleFramesInOneWebSocketMessage(t *testing.T) {
	s := newTestHub(t)
	conn := dialTestClient(t, startTestServer(t, s))

	// CONNECT 与 SUBSCRIBE 挤在同一个 WS 消息里。
	payload := "CONNECT\naccept-version:1.2\nhost:/\n\n\x00" +
		"SUBSCRIBE\ndestination:/topic/room\nid:sub-1\nreceipt:r1\n\n\x00"
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}

	expectBoth(t, conn, 2, frame.CONNECTED, frame.RECEIPT)
}

// 一个 STOMP 帧可以跨多个 WebSocket 消息（客户端分片发送，如 stompjs 的 splitLargeFrames）。
func TestFrameSplitAcrossWebSocketMessages(t *testing.T) {
	s := newTestHub(t)
	conn := dialTestClient(t, startTestServer(t, s))

	if err := conn.WriteMessage(websocket.TextMessage, []byte("CONN")); err != nil {
		t.Fatalf("write part 1: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ECT\naccept-version:1.2\nhost:/\n\n\x00")); err != nil {
		t.Fatalf("write part 2: %v", err)
	}

	expectFrame(t, conn, frame.CONNECTED)
}

// 投递正在进行时硬断开：退回的动作不能偷看 sub.frame（无同步读），也不能把
// 还压在通道里的帧交给第二条连接。这条配合 -race 看。
func TestQueueDisconnectDuringDelivery(t *testing.T) {
	s := newTestHub(t, WithEventHooks(&routeHook{}))
	url := startTestServer(t, s)

	sender := dialTestClient(t, url)
	connect(t, sender, "")

	// 多个发送者持续灌同一个队列，让投递与断开真正重叠。
	for w := 0; w < 3; w++ {
		go func() {
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			if err := writeRaw(conn, frame.CONNECT, map[string]string{
				"accept-version": "1.2",
				"host":           "/",
			}); err != nil {
				return
			}
			for {
				if err := writeRaw(conn, frame.SEND, map[string]string{"destination": "/queue/race"}); err != nil {
					return
				}
			}
		}()
	}

	for i := 0; i < 30; i++ {
		victim := dialTestClient(t, url)
		connect(t, victim, "")

		// 刻意用 ack:auto 且订阅后不再读：它会一条条取走队列帧并立刻回到池子，
		// 从而与 processor 写 sub.frame 真正重叠（ack:client 拿一条就进 subList）。
		writeSTOMPFrame(t, victim, frame.SUBSCRIBE, map[string]string{
			"destination": "/queue/race",
			"id":          "sub-1",
		})
		time.Sleep(5 * time.Millisecond)

		// 不等它读完，直接把连接硬断掉。
		_ = victim.Close()
	}
}

// ──────────────── 广播 ────────────────

// 三个用户进同一个房间：任意一人发言，三人都应收到（含发言者自己）。
func TestRelayBroadcastReachesEverySubscriber(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	const users = 3
	room := make([]*websocket.Conn, users)
	for i := range room {
		room[i] = dialAndSubscribe(t, url, "/topic/room", fmt.Sprintf("sub-%d", i), nil)
	}

	// 第一个用户发言。
	sendWithBody(t, room[0], "/topic/room", "hello everyone")

	for i, conn := range room {
		headers := expectFrame(t, conn, frame.MESSAGE)
		if got := headers[frame.Destination]; got != "/topic/room" {
			t.Errorf("用户 %d: destination = %q, want /topic/room", i, got)
		}
		if got := headers[frame.Subscription]; got != fmt.Sprintf("sub-%d", i) {
			t.Errorf("用户 %d: subscription = %q, want sub-%d", i, got, i)
		}
	}
}

// 默认 ack（auto）的 topic 订阅者连收两条广播，必须**恰好**收到两条。
//
// 这是「投递完成后挂回队列池只对队列有意义」这条不变量的哨兵：该判定已从连接层
// 挪到中继侧（连接层不再判目的地），前提是 queue 帧走 subChan、topic 帧走 writeChan。
// 若哪天 topic 帧被误走 subChan，就会触发一次多余的挂载 —— 订阅在 topic 名单里翻倍，
// 一条消息收两次，并且随投递次数越滚越多。
func TestRelayTopicSubscriberIsNotDuplicatedOnDelivery(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	sub := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/topic/room", "first")
	sendWithBody(t, sender, "/topic/room", "second")

	for _, want := range []string{"first", "second"} {
		_, _, body := readSTOMPFrame(t, sub)
		if body != want {
			t.Fatalf("收到正文 %q, want %q", body, want)
		}
	}

	if extra := readUntilQuiet(t, sub, 400*time.Millisecond); len(extra) != 0 {
		t.Errorf("两条广播之后又收到 %v —— topic 订阅被重复挂载了", extra)
	}
}

// ──────────────── 队列 ────────────────

// 两个工作者订阅同一队列：四条消息应当各只交给一个人 —— 合起来正好四条、没有重复。
func TestRelayQueueDeliversEachMessageOnce(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	workers := []*websocket.Conn{
		dialAndSubscribe(t, url, "/queue/jobs", "w-0", nil),
		dialAndSubscribe(t, url, "/queue/jobs", "w-1", nil),
	}

	sender := dialTestClient(t, url)
	connect(t, sender, "")

	const jobs = 4
	for i := 0; i < jobs; i++ {
		sendWithBody(t, sender, "/queue/jobs", fmt.Sprintf("job-%d", i))
	}

	delivered := map[string]int{}
	for _, w := range workers {
		for _, msg := range readUntilQuiet(t, w, 400*time.Millisecond) {
			if msg[0] == frame.MESSAGE {
				delivered[msg[2]]++
			}
		}
	}

	for i := 0; i < jobs; i++ {
		body := fmt.Sprintf("job-%d", i)
		if delivered[body] != 1 {
			t.Errorf("消息 %q 被投递 %d 次，want 1（队列是一条消息只给一个消费者）", body, delivered[body])
		}
	}
}

// ──────────────── 私聊 ────────────────

// 私聊：只有目标用户的队列收到，别的用户收不到。
func TestRelayPrivateMessageReachesOnlyTarget(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	alice := dialAndSubscribe(t, url, "/queue/user/alice", "alice-sub", nil)
	bob := dialAndSubscribe(t, url, "/queue/user/bob", "bob-sub", nil)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/queue/user/alice", "hi alice")

	headers := expectFrame(t, alice, frame.MESSAGE)
	if got := headers[frame.Subscription]; got != "alice-sub" {
		t.Errorf("alice 收到的 subscription = %q, want alice-sub", got)
	}

	if got := readUntilQuiet(t, bob, 400*time.Millisecond); len(got) != 0 {
		t.Errorf("bob 收到了 %v —— 私聊不该投给非目标用户", got)
	}
}

// ──────────────── NACK ────────────────

// NACK 把未确认的消息放回**队首**：它应当排在更早就在队列里的那条之前被重新投递。
func TestRelayNackRequeuesToHead(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	// ACK/NACK 在 1.1 用 message-id（1.2 用 id）。
	worker := dialTestClient(t, url)
	writeSTOMPFrame(t, worker, frame.CONNECT, map[string]string{"accept-version": "1.1", "host": "/"})
	expectFrame(t, worker, frame.CONNECTED)
	writeSTOMPFrame(t, worker, frame.SUBSCRIBE, map[string]string{
		"destination": "/queue/jobs", "id": "w-0", "ack": frame.AckClient, "receipt": "r-0",
	})
	expectFrame(t, worker, frame.RECEIPT)

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/queue/jobs", "first")
	sendWithBody(t, sender, "/queue/jobs", "second")

	// 第一条先到（ack:client，未经确认不会再给第二条）→ 拒绝它。
	headers := expectFrame(t, worker, frame.MESSAGE)
	writeSTOMPFrame(t, worker, frame.NACK, map[string]string{"message-id": headers[frame.MessageId]})

	_, _, body := readSTOMPFrame(t, worker)
	if body != "first" {
		t.Errorf("NACK 后重新投递的是 %q, want %q（应放回队首，而不是排到队尾）", body, "first")
	}
}

// ──────────────── 事务 ────────────────

// 事务：SEND 进事务时不投递，COMMIT 之后才投递给订阅者。
func TestRelayTransactionDeliversOnCommit(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	subscriber := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)

	// 静止性用**另一条连接**观察：gorilla 的连接一旦读超时状态即损坏，
	// 拿同一把连接先做静止检查会把它读坏，后面就再也读不到东西。
	watcher := dialAndSubscribe(t, url, "/topic/room", "watch-1", nil)

	tx := dialTestClient(t, url)
	connect(t, tx, "")
	writeSTOMPFrame(t, tx, frame.BEGIN, map[string]string{"transaction": "tx1"})
	sendWithBodyHeaders(t, tx, "/topic/room", "in transaction", map[string]string{"transaction": "tx1"})

	if got := readUntilQuiet(t, watcher, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("提交前就收到了 %v —— 事务内的 SEND 不该立即投递", got)
	}

	writeSTOMPFrame(t, tx, frame.COMMIT, map[string]string{"transaction": "tx1"})

	_, _, body := readSTOMPFrame(t, subscriber)
	if body != "in transaction" {
		t.Errorf("提交后收到的正文 = %q, want %q", body, "in transaction")
	}
}

// ──────────────── 顺序 ────────────────

// 一个订阅者按序收多条：队列是 FIFO。
func TestRelayMessagesArriveInOrder(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	worker := dialAndSubscribe(t, url, "/queue/jobs", "w-0", nil)
	sender := dialTestClient(t, url)
	connect(t, sender, "")

	const count = 5
	for i := 0; i < count; i++ {
		sendWithBody(t, sender, "/queue/jobs", fmt.Sprintf("m-%d", i))
	}

	for i := 0; i < count; i++ {
		_, _, body := readSTOMPFrame(t, worker)
		if want := fmt.Sprintf("m-%d", i); body != want {
			t.Fatalf("第 %d 条是 %q, want %q（队列应按 FIFO 投递）", i+1, body, want)
		}
	}
}

// 客户端自己会发 STOMP 心跳（一个 "\n"）：服务端要当心跳处理，不能当成坏帧，
// 之后这条连接还得能正常用。
func TestCompatClientHeartBeatsAreNotFrames(t *testing.T) {
	url := startTestServer(t, newTestHub(t))
	conn := dialTestClient(t, url)
	connectLikeStompjs(t, conn)

	for i := 0; i < 3; i++ {
		if err := conn.WriteMessage(websocket.TextMessage, []byte("\n")); err != nil {
			t.Fatalf("write heart-beat: %v", err)
		}
	}

	// 心跳之后连接仍可用。
	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
		"destination": "/topic/room", "id": "sub-1", "receipt": "r-1",
	})
	expectFrame(t, conn, frame.RECEIPT)
}

// 前端一条连接同时订阅多个目的地（广播 + 在线人数 + 私聊），各收各的。
func TestCompatMultipleSubscriptionsOnOneConnection(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	conn := dialTestClient(t, url)
	connectLikeStompjs(t, conn)

	destinations := []string{"/topic/system/broadcast", "/topic/system/online", "/queue/user/uid-1"}
	for i, dest := range destinations {
		writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
			"destination": dest, "id": fmt.Sprintf("sub-%d", i), "receipt": fmt.Sprintf("r-%d", i),
		})
		expectFrame(t, conn, frame.RECEIPT)
	}

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/topic/system/broadcast", "broadcast")
	sendWithBody(t, sender, "/queue/user/uid-1", "private")

	// 两条各自只投给对应的订阅，且 subscription 头是各自的 id。
	first := expectFrame(t, conn, frame.MESSAGE)
	if first[frame.Subscription] != "sub-0" {
		t.Errorf("广播的 subscription = %q, want sub-0", first[frame.Subscription])
	}
	second := expectFrame(t, conn, frame.MESSAGE)
	if second[frame.Subscription] != "sub-2" {
		t.Errorf("私聊的 subscription = %q, want sub-2", second[frame.Subscription])
	}
}

// 大消息：客户端按 16KB 切片发送（stompjs 的 splitLargeFrames 就是这么干的），
// 服务端要按 content-length 把 body 拼回来并完整投递。
func TestCompatLargeBodySplitAcrossMessages(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&routeHook{})))

	subscriber := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)
	sender := dialTestClient(t, url)
	connectLikeStompjs(t, sender)

	// 构造一条 ~100KB 的正文，按 16KB 切开发送。
	body := strings.Repeat("x", 100*1024)
	payload := fmt.Sprintf("SEND\ndestination:/topic/room\ncontent-length:%d\n\n%s\x00", len(body), body)

	const chunk = 16 * 1024
	for start := 0; start < len(payload); start += chunk {
		end := start + chunk
		if end > len(payload) {
			end = len(payload)
		}
		if err := sender.WriteMessage(websocket.TextMessage, []byte(payload[start:end])); err != nil {
			t.Fatalf("write chunk %d: %v", start/chunk, err)
		}
	}

	_, _, got := readSTOMPFrame(t, subscriber)
	if len(got) != len(body) {
		t.Fatalf("收到正文 %d 字节, want %d（大 body 跨消息要正确拼回）", len(got), len(body))
	}
	if got != body {
		t.Error("正文内容不一致")
	}
}
