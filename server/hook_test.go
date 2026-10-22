// 事件钩子与在线追踪：回调是否按预期发生、钩子里 panic 的隔离、关停时的下线通知。
package server

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
)

// 断开清理里调用的 hook/tracker 若 panic，不得让整个进程崩掉（清理路径逃出了
// processLoop 的 recover）。
func TestPanicInDisconnectHookDoesNotKillProcess(t *testing.T) {
	hook := &panicHook{onDisconnect: true}
	s := newTestHub(t, WithEventHooks(hook))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")
	_ = conn.Close() // 触发断开 → OnDisconnect panic

	// 进程必须还活着：新连接仍能正常握手。
	time.Sleep(50 * time.Millisecond)
	again := dialTestClient(t, url)
	connect(t, again, "")
}

// processLoop 里的 panic 被 recover 之后，连接必须被关掉 —— 否则它变成僵尸：
// 不再处理任何帧、也收不到任何消息，socket 却还开着，readLoop/writeLoop 一起泄漏。
func TestPanicInHookClosesConnection(t *testing.T) {
	hook := &panicHook{onSubscribe: true}
	s := newTestHub(t, WithEventHooks(hook))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")
	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
		"destination": "/topic/room",
		"id":          "sub-1",
	})

	// 期望：服务端把这条连接关掉（读到的是关闭而不是超时）。
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("ReadMessage() = nil error，服务端没有关掉这条连接")
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("hook panic 后连接成了僵尸：既没关也没报错，客户端只能等到超时")
	}
}

// 完整生命周期：连接 → 订阅 → 广播 → 退订 → 再广播 → 断开，
// 每一步都断言服务端侧的回调与在线追踪确实发生了。
func TestLifecycleConnectSubscribeUnsubscribeDisconnect(t *testing.T) {
	hooks := &recordingHook{}
	presence := &countingPresence{}

	// recordingHook 排在 routeHook 之前：hook 链遇到第一个返回 false 的就中断，
	// 所以负责路由的 routeHook 要放在记录用 hook 之后。
	s := newTestHub(t, WithEventHooks(hooks, &routeHook{}), WithPresenceManager(presence))
	url := startTestServer(t, s)

	alice := dialAndSubscribe(t, url, "/topic/room", "alice-sub", nil)
	bob := dialAndSubscribe(t, url, "/topic/room", "bob-sub", nil)

	waitFor(t, func() int { return hooks.count(&hooks.connects) }, 2, "OnConnect")
	waitFor(t, func() int { return hooks.count(&hooks.subscribes) }, 2, "OnSubscribe")
	// 断言的是「上线」（续期要等在线同步周期，不适合在这里断言）。
	if got := presence.joinCount(); got != 2 {
		t.Errorf("presence 收到 %d 次上线, want 2", got)
	}

	// alice 发言：两人都收到，OnSend 一次。
	sendWithBody(t, alice, "/topic/room", "hi")
	expectFrame(t, alice, frame.MESSAGE)
	expectFrame(t, bob, frame.MESSAGE)
	waitFor(t, func() int { return hooks.count(&hooks.sends) }, 1, "OnSend")

	// alice 退订：之后只有 bob 收到。
	writeSTOMPFrame(t, alice, frame.UNSUBSCRIBE, map[string]string{"id": "alice-sub", "receipt": "unsub-1"})
	expectFrame(t, alice, frame.RECEIPT) // 退订回执要消费掉，否则会被当成「退订后仍收到东西」
	waitFor(t, func() int { return hooks.count(&hooks.unsubscribes) }, 1, "OnUnsubscribe")

	sendWithBody(t, bob, "/topic/room", "only bob now")
	expectFrame(t, bob, frame.MESSAGE)
	if got := readUntilQuiet(t, alice, 400*time.Millisecond); len(got) != 0 {
		t.Errorf("已退订的 alice 仍收到 %v", got)
	}

	// 双方断开：OnDisconnect 各一次。
	_ = alice.Close()
	_ = bob.Close()
	waitFor(t, func() int { return hooks.count(&hooks.disconnects) }, 2, "OnDisconnect")
}

// 服务端关停时，在线客户端应当收到 ERROR（而不是连接莫名断掉）—— 这是滚动更新时
// 前端能看到的东西，也是它决定「立刻重连」的依据。
func TestLifecycleShutdownNotifiesConnectedClients(t *testing.T) {
	s := newTestHub(t)
	url := startTestServer(t, s)

	conn := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)

	if err := s.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	headers := expectFrame(t, conn, frame.ERROR)
	if msg := headers[frame.Message]; !strings.Contains(msg, "shutting down") {
		t.Errorf("ERROR message = %q, want 含 shutting down", msg)
	}

	// 随后连接应当被关闭。
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("关停后连接没有被断开")
	}
}

// 关页面走 DISCONNECT：服务端要完成清理（退订 + 断开回调），之后不再投消息给它。
func TestCompatDisconnectCleansUpSubscription(t *testing.T) {
	hooks := &recordingHook{}
	url := startTestServer(t, newTestHub(t, WithEventHooks(hooks, &routeHook{})))

	leaving := dialAndSubscribe(t, url, "/topic/room", "leaving-sub", nil)
	waitFor(t, func() int { return hooks.count(&hooks.connects) }, 1, "OnConnect")

	writeSTOMPFrame(t, leaving, frame.DISCONNECT, map[string]string{"receipt": "bye-1"})
	expectFrame(t, leaving, frame.RECEIPT)
	waitFor(t, func() int { return hooks.count(&hooks.disconnects) }, 1, "OnDisconnect")
	// 规范：客户端收完 RECEIPT 后不得再发帧；服务端刷出 RECEIPT 即关闭。
	expectClosed(t, leaving)

	// 退订已生效：之后的广播不再投给这条连接。
	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/topic/room", "after leaving")

	if got := readUntilQuiet(t, leaving, 400*time.Millisecond); len(got) != 0 {
		t.Errorf("已 DISCONNECT 的连接仍收到 %v", got)
	}
}

// 断线重连（前端最常见的路径）：以同一 login 重新连上并重新订阅，之后仍能收到消息。
func TestCompatReconnectSameLogin(t *testing.T) {
	hooks := &recordingHook{}
	presence := &countingPresence{}
	url := startTestServer(t, newTestHub(t, WithEventHooks(hooks, &routeHook{}), WithPresenceManager(presence)))

	first := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)
	_ = first.Close()
	waitFor(t, func() int { return hooks.count(&hooks.disconnects) }, 1, "OnDisconnect")

	// 同一个人重连（stompjs 会自动这么做），重新订阅。
	second := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)
	waitFor(t, func() int { return hooks.count(&hooks.connects) }, 2, "OnConnect")

	sender := dialTestClient(t, url)
	connect(t, sender, "")
	sendWithBody(t, sender, "/topic/room", "welcome back")

	_, _, body := readSTOMPFrame(t, second)
	if body != "welcome back" {
		t.Errorf("重连后收到的正文 = %q, want %q", body, "welcome back")
	}
}
