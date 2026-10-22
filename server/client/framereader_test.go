package client

// 帧从 WebSocket 消息流里读出来时的字节透传与两种上限（头段、正文）。

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// ──────────────── 帧上限（wsFrameReader） ────────────────

// newTestFrameReader 起一个裸 WS 服务端，把**服务端侧**的连接交给 wsFrameReader；
// 返回读者与「把字节喂进去」的函数（chunk 是每条 WS 消息的字节数，1 即逐字节分片）。
// 喂完即关连接 —— 读者随之结束。
func newTestFrameReader(t *testing.T, maxBody, maxHeader int) (*wsFrameReader, func(string, int) error) {
	t.Helper()

	srvConn := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConn <- conn
		<-done // 连接归读者独占（gorilla 只允许一个读者），这里只保持连接
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })

	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })

	// feed 把数据按 chunk 切成 WS 消息发出去，最后关连接（读者借此收尾）。
	// 返回错误交给调用方忽略 —— 超限用例里读者会**提前**失败，写端此时写不进去是预期的。
	feed := func(data string, chunk int) error {
		for i := 0; i < len(data); i += chunk {
			end := i + chunk
			if end > len(data) {
				end = len(data)
			}
			if err := peer.WriteMessage(websocket.TextMessage, []byte(data[i:end])); err != nil {
				return err
			}
		}
		return peer.Close()
	}
	return newWSFrameReader(<-srvConn, maxBody, maxHeader), feed
}

// drainAll 读完读者的全部输出（到出错为止）。
func drainAll(r io.Reader) ([]byte, error) {
	var out []byte
	buf := make([]byte, 7) // 故意错开缓冲边界
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
	}
}

// 头段与正文各有上限：正常帧**逐字节**透传，超限帧在正文分配之前就报错。
func TestWSFrameReader(t *testing.T) {
	frameWithBody := "SEND\ndestination:/queue/a\ncontent-length:5\n\nhello\x00"
	noLength := "SEND\ndestination:/queue/a\n\nbye\x00"
	crlf := "SEND\r\ndestination:/queue/a\r\ncontent-length:2\r\n\r\nhi\x00"
	twoFrames := frameWithBody + "SEND\ndestination:/queue/b\n\nbye\x00"
	afterEOLs := frameWithBody + "\n\n" + noLength
	bodyWithNUL := "SEND\ndestination:/queue/a\ncontent-length:3\n\na\x00b\x00"

	cases := []struct {
		name    string
		in      string
		maxBody int
		wantErr bool
	}{
		{"正常帧", frameWithBody, 1024, false},
		{"CRLF 行尾", crlf, 1024, false},
		{"无 content-length（NUL 结尾）", noLength, 1024, false},
		{"一次读到多帧", twoFrames, 1024, false},
		{"NUL 后跟多个 EOL 再跟下一帧", afterEOLs, 1024, false},
		{"正文内含 NUL（按 content-length 定界）", bodyWithNUL, 1024, false},
		{"空正文", "SEND\ndestination:/queue/a\ncontent-length:0\n\n\x00", 1024, false},
		{"命令前的多个心跳行", "\n\n\n" + noLength, 1024, false},
		{"声明超上限", "SEND\ndestination:/queue/a\ncontent-length:2000000000\n\n", 1024, true},
		{"无 content-length 且正文超上限", "SEND\ndestination:/queue/a\n\n" + strings.Repeat("x", 2000) + "\x00", 1024, true},
		{"头段超上限", "SEND\n" + strings.Repeat("x:y\n", 4000), 1 << 20, true},
		{
			// 上限按**帧**算：三帧各 10 字节正文，累计 30 > 16 也不该报错。
			"多帧各自不超限、累计超限",
			"SEND\ndestination:/queue/a\ncontent-length:10\n\n" + strings.Repeat("a", 10) + "\x00" +
				"\n\n" +
				"SEND\ndestination:/queue/a\ncontent-length:10\n\n" + strings.Repeat("b", 10) + "\x00" +
				"SEND\ndestination:/queue/a\ncontent-length:10\n\n" + strings.Repeat("c", 10) + "\x00",
			16, false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 两种喂法都要过：一条消息给足 / 每条消息只给一字节（跨消息边界最容易出错）
			for _, chunk := range []int{len(tc.in), 1} {
				r, feed := newTestFrameReader(t, tc.maxBody, 8<<10)
				go func() { _ = feed(tc.in, chunk) }()

				got, err := drainAll(r)
				if tc.wantErr {
					if !errors.Is(err, errFrameTooLarge) {
						t.Fatalf("chunk=%d: 错误 = %v, want errFrameTooLarge（读到 %d 字节）", chunk, err, len(got))
					}
					continue
				}
				if string(got) != tc.in {
					t.Fatalf("chunk=%d: 透传字节不一致\n got %q\nwant %q", chunk, got, tc.in)
				}
			}
		})
	}
}
