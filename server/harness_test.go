// 本包的测试床：起真实 WebSocket 服务端、拨真连接、收发与断言 STOMP 帧。
// 各测试文件共用，故独立成文件 —— 否则辅助函数会寄居在第一个用到它的用例文件里。
package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"

	"github.com/ve-weiyi/stompws/logws"
)

// newTestHub 创建一个用完自动关停的服务器。日志默认静音，调用方传入的
// WithLogger 排在最后，因此可以覆盖默认值。
func newTestHub(t *testing.T, opts ...ServerOption) *StompHubServer {
	t.Helper()

	opts = append([]ServerOption{WithLogger(&logws.NoOpLogger{})}, opts...)
	s := NewStompHubServer(opts...)
	t.Cleanup(func() {
		_ = s.Shutdown(time.Second)
	})
	return s
}

// startTestServer 把服务器挂到一个真实的 HTTP 服务上，返回 WebSocket 地址。
func startTestServer(t *testing.T, s *StompHubServer) string {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWebSocket)

	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	return "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
}

// dialTestClient 连上一个测试服务端。
func dialTestClient(t *testing.T, url string) *websocket.Conn {
	t.Helper()

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// 测试用的短超时：把心跳下限与 CONNECT 容忍时长压到毫秒级，相关判定才落在测试窗口内。
// 走 Option 注入而不是改包级变量 —— 于是这些测试不再需要串行。
const (
	testHeartBeat      = 50 * time.Millisecond
	testConnectTimeout = 100 * time.Millisecond
)

// writeRaw 按 STOMP 线格式写一个帧。返回错误而不是断言 —— 并发测试里
// 干活的是普通协程，而 t.Fatalf 只能从测试主协程调用。
func writeRaw(conn *websocket.Conn, command string, headers map[string]string) error {
	var b strings.Builder
	b.WriteString(command)
	b.WriteByte('\n')
	for k, v := range headers {
		b.WriteString(k + ":" + v + "\n")
	}
	b.WriteString("\n\x00")

	return conn.WriteMessage(websocket.TextMessage, []byte(b.String()))
}

// writeSTOMPFrame 是 writeRaw 的断言版。
func writeSTOMPFrame(t *testing.T, conn *websocket.Conn, command string, headers map[string]string) {
	t.Helper()

	if err := writeRaw(conn, command, headers); err != nil {
		t.Fatalf("write %s frame: %v", command, err)
	}
}

// readSTOMPFrame 读一个帧并拆出命令、头部与正文。
func readSTOMPFrame(t *testing.T, conn *websocket.Conn) (string, map[string]string, string) {
	t.Helper()

	command, headers, body, err := readFrameRaw(conn)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return command, headers, body
}

// readFrameRaw 读一个帧并拆出命令、头部与正文。返回错误而不是断言 ——
// 并发测试里干活的是普通协程，而 t.Fatalf 只能从测试主协程调用。
func readFrameRaw(conn *websocket.Conn) (string, map[string]string, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return "", nil, "", err
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return "", nil, "", err
	}
	command, headers, body := parseFrame(data)
	return command, headers, body, nil
}

// parseFrame 拆一条原始帧（不含读 deadline，方便调用方自己定静默窗口）。
func parseFrame(data []byte) (string, map[string]string, string) {
	head, body, _ := strings.Cut(string(data), "\n\n")
	lines := strings.Split(head, "\n")

	headers := make(map[string]string, len(lines)-1)
	for _, line := range lines[1:] {
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[k] = v
		}
	}
	return lines[0], headers, strings.TrimSuffix(body, "\x00")
}

// connect 发送 CONNECT，返回服务端 CONNECTED 帧的头部。
func connect(t *testing.T, conn *websocket.Conn, heartBeat string) map[string]string {
	t.Helper()

	headers := map[string]string{
		"accept-version": "1.2",
		"host":           "/",
	}
	if heartBeat != "" {
		headers["heart-beat"] = heartBeat
	}
	writeSTOMPFrame(t, conn, frame.CONNECT, headers)

	return expectFrame(t, conn, frame.CONNECTED)
}

// expectFrame 读一个帧并断言它的命令，返回头部。
func expectFrame(t *testing.T, conn *websocket.Conn, wantCommand string) map[string]string {
	t.Helper()

	command, headers, _ := readSTOMPFrame(t, conn)
	if command != wantCommand {
		t.Fatalf("读到 %q (headers %v), want %q", command, headers, wantCommand)
	}
	return headers
}

// expectBoth 读 count 个帧，断言其中这两个命令都出现过（顺序不定）。
func expectBoth(t *testing.T, conn *websocket.Conn, count int, wants ...string) {
	t.Helper()

	seen := make(map[string]int, count)
	for i := 0; i < count; i++ {
		command, headers, _ := readSTOMPFrame(t, conn)
		seen[command]++
		if seen[command] > 1 {
			t.Fatalf("第 %d 帧重复读到 %q (headers %v)，want %v", i+1, command, headers, wants)
		}
	}

	for _, w := range wants {
		if seen[w] == 0 {
			t.Errorf("没读到 %q，只读到 %v", w, seen)
		}
	}
}

// ──────────────── 线缆辅助 ────────────────

// dialAndSubscribe 连上、握手、订阅一个目的地并等到回执。
func dialAndSubscribe(t *testing.T, url, destination, subID string, extra map[string]string) *websocket.Conn {
	t.Helper()

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	headers := map[string]string{
		"destination": destination,
		"id":          subID,
		"receipt":     subID + "-receipt",
	}
	for k, v := range extra {
		headers[k] = v
	}

	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, headers)
	expectFrame(t, conn, frame.RECEIPT)
	return conn
}

// sendWithBody 发一条带正文的 SEND（正文用于区分不同消息）。
func sendWithBody(t *testing.T, conn *websocket.Conn, destination, body string) {
	t.Helper()
	sendWithBodyHeaders(t, conn, destination, body, nil)
}

// sendWithBodyHeaders 同上，可附带额外头部（如 transaction）。
func sendWithBodyHeaders(t *testing.T, conn *websocket.Conn, destination, body string, extra map[string]string) {
	t.Helper()

	var b strings.Builder
	b.WriteString("SEND\n")
	b.WriteString("destination:" + destination + "\n")
	b.WriteString(fmt.Sprintf("content-length:%d\n", len(body)))
	for k, v := range extra {
		b.WriteString(k + ":" + v + "\n")
	}
	b.WriteString("\n")
	b.WriteString(body)
	b.WriteString("\x00")

	if err := conn.WriteMessage(websocket.TextMessage, []byte(b.String())); err != nil {
		t.Fatalf("send to %s: %v", destination, err)
	}
}

// readUntilQuiet 读到静默为止，返回期间收到的 (命令, 头, 正文) 列表。
//
// 注意：它靠「读超时」判断静默，而 gorilla 的连接**读超时之后状态即损坏**（后续读一律报错），
// 所以只能在一条连接的**最后一次**读取上用；还要继续读的连接要换一条来观察。
func readUntilQuiet(t *testing.T, conn *websocket.Conn, quiet time.Duration) [][3]string {
	t.Helper()

	var got [][3]string
	for {
		// 自带 deadline：readFrameRaw 会把 deadline 固定成 2s，静默窗口就失效了。
		if err := conn.SetReadDeadline(time.Now().Add(quiet)); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return got // 静默或对端关闭
		}
		command, headers, body := parseFrame(data)
		got = append(got, [3]string{command, headers[frame.Destination], body})
	}
}

// waitFor 轮询等计数达标：回调跑在连接的协程上，客户端读到回执不代表回调已经结束。
func waitFor(t *testing.T, get func() int, want int, what string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := get(); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 计数 = %d, want %d", what, get(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 按**前端实际发的东西**构造帧，而不是按规范应该发的东西。
//
// 依据（blog-app 实际用法）：
//   - 客户端库是 @stomp/stompjs，它自动加 `accept-version:1.0,1.1,1.2` 与
//     `heart-beat:<outgoing>,<incoming>`（前端配的是 10000,10000），
//     但**不会**自动加 `host` —— 所以两个前端的 `connectHeaders` 里各显式补了一行。
//   - 订阅的目的地形态：`/topic/system/broadcast`、`/topic/system/online`、`/queue/user/<id>`。
//   - 关页面走 `deactivate()`，即发 DISCONNECT；断线由它自己按 reconnectDelay 重连。
//
// connectLikeStompjs 按前端（@stomp/stompjs）的形态发 CONNECT：三版本列表 + 心跳 +
// 自定义头 + host。
//
// host 必须显式给：STOMP 1.1/1.2 规定 CONNECT 的 host 是 REQUIRED，而 stompjs
// **不会**自动加它（它只加 accept-version 与 heart-beat）。两个前端都显式补了这一行。
func connectLikeStompjs(t *testing.T, conn *websocket.Conn) map[string]string {
	t.Helper()

	writeSTOMPFrame(t, conn, frame.CONNECT, map[string]string{
		"accept-version": "1.0,1.1,1.2",
		"heart-beat":     "10000,10000", // 前端 WS_CONFIG 的值
		"host":           "/",
		"login":          "uid-1",
		"passcode":       "token-1",
		"client":         "device-1",
	})
	return expectFrame(t, conn, frame.CONNECTED)
}

// expectClosed 断言服务端关闭了这条连接：先读到的东西（通常是 ERROR 帧）可以被跳过，
// 但最终必须是「连接被关闭」，而不是客户端自己读到超时。
func expectClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("服务端没有关闭连接（客户端只是自己读超时了）")
			}
			return
		}
	}
}
