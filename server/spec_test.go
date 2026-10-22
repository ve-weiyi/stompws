// 按 STOMP 1.2 规范条目组织的合规测试：只放「规范原文这么要求，所以要这么断言」的用例，
// 每条都标出规范出处。
package server

import (
	"strings"
	"testing"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"
)

// 前端形态的 CONNECT（含 host）必须被接受，并按三版本列表取最高的 1.2。
func TestCompatStompjsConnectShapeIsAccepted(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	headers := connectLikeStompjs(t, conn)

	if got := headers[frame.Version]; got != "1.2" {
		t.Errorf("CONNECTED version = %q, want 1.2（三版本列表里取最高）", got)
	}
}

// 1.1/1.2 的 CONNECT 缺 host 按规范被拒 —— 服务端不为迁就某个客户端的默认行为放宽契约。
func TestCompatConnectWithoutHostIsRejected(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	writeSTOMPFrame(t, conn, frame.CONNECT, map[string]string{
		"accept-version": "1.0,1.1,1.2",
		"heart-beat":     "10000,10000",
		"login":          "uid-1",
		"passcode":       "token-1",
	})

	headers := expectFrame(t, conn, frame.ERROR)
	if msg := headers[frame.Message]; !strings.Contains(msg, frame.Host) {
		t.Errorf("ERROR message = %q, want 指出缺 %s", msg, frame.Host)
	}
	// 规范：拒绝 CONNECT 时 SHOULD 回 ERROR 并关闭。
	expectClosed(t, conn)
}

// 心跳：前端请求 10000，服务端下限是 30s，所以 CONNECTED 里应当回 30000。
func TestCompatHeartBeatFloorApplies(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	headers := connectLikeStompjs(t, conn)

	// 服务端回的是 <sx>,<sy>，两者都被抬到下限。
	if got := headers[frame.HeartBeat]; got != "30000,30000" {
		t.Errorf("CONNECTED heart-beat = %q, want 30000,30000（10000 被抬到下限）", got)
	}
}

// 规范 §二 头表：各帧的 REQUIRED 头缺失时，服务端无法处理该帧。
// 规范 §五：无法处理 → MUST 回 ERROR 并关闭连接。
func TestSpecRequiredHeadersMissingIsFatal(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, conn *websocket.Conn)
		command string
		headers map[string]string
	}{
		{
			name:    "SEND 缺 destination",
			command: frame.SEND,
			headers: nil,
		},
		{
			name:    "SUBSCRIBE 缺 id",
			command: frame.SUBSCRIBE,
			headers: map[string]string{"destination": "/topic/room"},
		},
		{
			name:    "UNSUBSCRIBE 缺 id",
			command: frame.UNSUBSCRIBE,
			headers: nil,
		},
		{
			name:    "BEGIN 缺 transaction",
			command: frame.BEGIN,
			headers: nil,
		},
		{
			name:    "1.2 的 ACK 缺 id",
			command: frame.ACK,
			headers: nil,
		},
		{
			name:    "CONNECT 带 receipt（CONNECT 不允许回执）",
			command: frame.CONNECT,
			headers: map[string]string{"accept-version": "1.2", "host": "/", "receipt": "r"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := startTestServer(t, newTestHub(t))

			conn := dialTestClient(t, url)
			if tc.command != frame.CONNECT {
				connect(t, conn, "")
			}

			writeSTOMPFrame(t, conn, tc.command, tc.headers)

			expectFrame(t, conn, frame.ERROR)
			expectClosed(t, conn)
		})
	}
}

// 规范 §五：「If the server cannot successfully create the subscription, the server
// MUST send an ERROR frame and then close the connection.」
func TestSpecSubscribeFailureIsFatal(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	// /foo/bar 过得了字符集校验，但不是 /topic/ 或 /queue/ —— 建不出这个订阅。
	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
		"destination": "/foo/bar", "id": "sub-1", "receipt": "r-1",
	})

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 规范 · SUBSCRIBE 的 ack 头只有 auto / client / client-individual 三种取值：
// 非法值 MUST ERROR + 关闭。
//
// 它必须在这里拦住，而不是留到 ACK 时才失效：非法值会让 IsAckedBy 永不匹配，
// 帧卡在 subList 里、队列订阅再也挂不回去 —— 客户端却因为收到 RECEIPT 以为一切正常。
func TestSpecSubscribeWithInvalidAckModeIsFatal(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
		"destination": "/queue/user/uid-1", "id": "sub-1", "ack": "bogus", "receipt": "r-1",
	})

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 规范 §五：SEND 无法处理（例如引用了不存在的事务）→ MUST ERROR + 关闭。
func TestSpecSendWithUnknownTransactionIsFatal(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	writeSTOMPFrame(t, conn, frame.SEND, map[string]string{
		"destination": "/topic/room", "transaction": "never-begun", "receipt": "r-1",
	})

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 规范 §一/§五：服务端可以对帧大小设限；超过限制 SHOULD ERROR + 关闭。
// 本实现把上限放在 WebSocket 读层（conn.SetReadLimit），见下方断言的说明。
func TestSpecOversizedMessageIsFatal(t *testing.T) {
	const limit = 4096

	url := startTestServer(t, newTestHub(t, WithMaxWSMessageSize(limit)))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	// 一条超过上限的 WebSocket 消息。
	big := make([]byte, limit+1024)
	for i := range big {
		big[i] = 'x'
	}
	if err := conn.WriteMessage(websocket.TextMessage, big); err != nil {
		t.Fatalf("write big message: %v", err)
	}

	// 客户端看到的是 WebSocket 层的 1009（message too big）：超出 SetReadLimit 时
	// gorilla 的读循环会自己先写一个关闭帧，STOMP 的 ERROR 帧只能尽力而为、会被它抢先。
	expectClosed(t, conn)
}

// 规范 §二 帧表：accept-version 对 CONNECT 与 STOMP 都是 REQUIRED。
// 缺头时**只有 CONNECT** 回落 1.0（规范：“If the accept-version header is missing, it means
// that the client only supports version 1.0 of the protocol”，且这一节只针对 CONNECT）；
// STOMP 帧是 1.1 才引入的，缺头即非法 —— 上游 determineVersion 同此。
func TestSpecStompFrameRequiresAcceptVersion(t *testing.T) {
	t.Run("CONNECT 缺 accept-version 按 1.0 放行", func(t *testing.T) {
		url := startTestServer(t, newTestHub(t))

		conn := dialTestClient(t, url)
		writeSTOMPFrame(t, conn, frame.CONNECT, map[string]string{"host": "/"})

		headers := expectFrame(t, conn, frame.CONNECTED)
		if got := headers[frame.Version]; got != stomp.V10.String() {
			t.Errorf("CONNECTED version = %q, want %q", got, stomp.V10.String())
		}
	})

	t.Run("STOMP 缺 accept-version 是非法帧", func(t *testing.T) {
		url := startTestServer(t, newTestHub(t))

		conn := dialTestClient(t, url)
		writeSTOMPFrame(t, conn, frame.STOMP, map[string]string{"host": "/"})

		expectFrame(t, conn, frame.ERROR)
		expectClosed(t, conn)
	})

	t.Run("STOMP 带 accept-version 正常连接", func(t *testing.T) {
		url := startTestServer(t, newTestHub(t))

		conn := dialTestClient(t, url)
		writeSTOMPFrame(t, conn, frame.STOMP, map[string]string{"accept-version": "1.2", "host": "/"})

		headers := expectFrame(t, conn, frame.CONNECTED)
		if got := headers[frame.Version]; got != "1.2" {
			t.Errorf("CONNECTED version = %q, want 1.2", got)
		}
	})
}

// 规范 §五：拒绝 CONNECT 时 SHOULD 回 ERROR 并关闭连接。
func TestSpecRejectedConnectClosesConnection(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	// 1.1/1.2 的 CONNECT 必填 host（规范 §二：clients MUST set host）。
	writeSTOMPFrame(t, conn, frame.CONNECT, map[string]string{"accept-version": "1.2"})

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 规范：RECEIPT 是「服务端已处理该帧（并累积到此前所有帧）」的确认，
// 所以 COMMIT 的回执只能发在提交成功之后 —— 否则客户端先收到「提交成功」，
// 再收到 ERROR，而事务其实没提交（其余帧还跟着事务一起被丢了）。
func TestSpecCommitReceiptFollowsCommit(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	// 提交一个不存在的事务：必然失败 → 只允许 ERROR，不能先冒出 RECEIPT。
	writeSTOMPFrame(t, conn, frame.COMMIT, map[string]string{
		"transaction": "never-begun", "receipt": "r-1",
	})

	headers := expectFrame(t, conn, frame.ERROR)
	if got := headers[frame.ReceiptId]; got != "r-1" {
		t.Errorf("ERROR receipt-id = %q, want r-1（出错帧带了 receipt）", got)
	}
	expectClosed(t, conn)
}

// 规范 §五：SEND 无法处理 → MUST ERROR + 关闭。目的地不是 /topic/ 或 /queue/ 就是这种情况
// （先前会伪造一条 MESSAGE —— 而 MESSAGE 按规范必须带 subscription，客户端根本没订阅它）。
//
// 注：ERROR 不带 receipt-id —— 发送方的 receipt 头在路由之前就被摘走了（那是防止它随
// MESSAGE 泄漏给订阅者所必需的），RouteMessage 因此拿不到它。
func TestSpecSendToInvalidDestinationIsFatal(t *testing.T) {
	url := startTestServer(t, newTestHub(t, WithEventHooks(&passthroughHook{})))

	conn := dialTestClient(t, url)
	connect(t, conn, "")
	writeSTOMPFrame(t, conn, frame.SEND, map[string]string{
		"destination": "/foo/bar", "receipt": "r-1",
	})

	expectFrame(t, conn, frame.ERROR)
	expectClosed(t, conn)
}

// 规范 §五：DISCONNECT 之后客户端不得再发帧，服务端在刷出 RECEIPT 后关闭连接。
func TestSpecDisconnectClosesServerSide(t *testing.T) {
	url := startTestServer(t, newTestHub(t))

	conn := dialAndSubscribe(t, url, "/topic/room", "sub-1", nil)

	writeSTOMPFrame(t, conn, frame.DISCONNECT, map[string]string{"receipt": "bye-1"})
	expectFrame(t, conn, frame.RECEIPT)

	expectClosed(t, conn)
}
