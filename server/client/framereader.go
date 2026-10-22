package client

// framereader.go 把一条 WebSocket 连接的消息流按 STOMP 帧的粒度喂给 frame.Reader，
// 同时守住帧的头段与正文上限。

import (
	"bytes"
	"strconv"

	"github.com/go-stomp/stomp/v3/frame"
	"github.com/gorilla/websocket"
)

// ──────────────── 从 WebSocket 消息读帧，并守住帧的上限 ────────────────

const (
	// maxFrameBody 是单条帧正文的上限，与单条 WebSocket 消息的上限一致。
	maxFrameBody = 1 << 20
	// maxHeaderBytes 是单条帧头段（命令 + 头 + 空行）的上限。
	maxHeaderBytes = 8 << 10
)

// 当前帧的正文状态。
const (
	bodyNone     = 0  // 帧之间：下一条要读的是头段
	bodyUntilNUL = -1 // 正文读到 NUL 为止（没有 content-length）
	bodyTail     = -2 // 计长正文已读完，下一个字节是终止 NUL
)

// wsFrameReader 把一条 WebSocket 连接的消息流按 STOMP 帧的粒度喂给 frame.Reader。
//
// 它做两件事，而这两件事必须在一起：**帧的边界与 WebSocket 消息的边界无关**（多帧合一、
// 一帧跨多消息（stompjs 的 splitLargeFrames）都要能解析），而**上限又必须按帧判定** ——
// frame.Reader 读完头就 make([]byte, contentLength)，content-length 由客户端给出，
// 一条 40 字节的 SEND+content-length:2000000000 足以让它申请 2GB；头段同理
// （readLine 用 ReadBytes，会无限累积）。
//
// 不变式：只改变字节交给调用方的**时机**（头段要等校验通过才放行），不改变字节本身。
type wsFrameReader struct {
	ws        *websocket.Conn
	maxBody   int
	maxHeader int

	msg  []byte // 当前 WS 消息里还没读走的字节（消息 → 字节流）
	out  []byte // 待转发：心跳行，或已校验通过的头段
	off  int
	body int // 正文状态：bodyNone、计长正文剩余（> 0）、bodyUntilNUL / bodyTail
	got  int // 无 content-length 时已放行的正文字节数
}

func newWSFrameReader(ws *websocket.Conn, maxBody, maxHeader int) *wsFrameReader {
	if maxBody <= 0 {
		maxBody = maxFrameBody
	}
	if maxHeader <= 0 {
		maxHeader = maxHeaderBytes
	}
	return &wsFrameReader{ws: ws, maxBody: maxBody, maxHeader: maxHeader}
}

// Read 实现 io.Reader，交给 frame.Reader 的就是它。
func (r *wsFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for {
		if r.off < len(r.out) { // ① 有就绪字节就先转发
			n := copy(p, r.out[r.off:])
			r.off += n
			return n, nil
		}
		if r.body != bodyNone { // ② 当前帧的正文
			return r.readBody(p)
		}
		if err := r.readHead(); err != nil { // ③ 读下一帧的头段（校验后进 out）
			return 0, err
		}
	}
}

// readHead 读一条帧的头段（命令到空行），确认合法后放进转发队列。
// 命令之前的空行是心跳，也原样转发（库把空行当心跳），转发完就交回调用方，避免攒着。
func (r *wsFrameReader) readHead() error {
	var head []byte
	for {
		n0 := len(head)
		var err error
		if head, err = r.readLine(head); err != nil {
			return err
		}
		blank := isBlankLine(head[n0:]) // 本行 = 本次读进来的这一段
		if blank && n0 == 0 {           // 命令之前的空行：心跳
			r.push(head)
			return nil
		}
		if blank { // 命令之后的空行：头段结束
			break
		}
	}

	// content-length 必须在**放行之前**判完 —— 放行之后库就会去分配正文。
	if n, ok := declaredContentLength(head); ok {
		if n > r.maxBody {
			return errFrameTooLarge
		}
		r.body = n
		if n == 0 {
			r.body = bodyTail
		}
	} else {
		r.body = bodyUntilNUL
	}
	r.push(head)
	return nil
}

// readBody 转发当前帧的正文：不缓冲，直接经过调用方的缓冲。
func (r *wsFrameReader) readBody(p []byte) (int, error) {
	if r.body == bodyTail { // 计长正文之后的终止字节（值不对由库报错）
		if err := r.fill(); err != nil {
			return 0, err
		}
		p[0] = r.msg[0]
		r.msg = r.msg[1:]
		r.body = bodyNone
		return 1, nil
	}

	if err := r.fill(); err != nil {
		return 0, err
	}

	if r.body == bodyUntilNUL {
		// 正文本身不能含 NUL，所以按 NUL 定界是安全的。
		end := len(r.msg)
		if i := bytes.IndexByte(r.msg, 0); i >= 0 {
			end = i + 1
		}
		if end > len(p) {
			end = len(p) // 只交调用方要的量，余下下次再交
		}
		n := copy(p, r.msg[:end])
		r.msg = r.msg[n:]

		r.got += n
		if r.got > r.maxBody {
			return n, errFrameTooLarge
		}
		if r.msg != nil && n > 0 && p[n-1] == 0 { // NUL 已交出，本帧结束
			r.body, r.got = bodyNone, 0
		}
		return n, nil
	}

	// 计长正文
	if len(p) > r.body {
		p = p[:r.body]
	}
	n := copy(p, r.msg)
	r.msg = r.msg[n:]
	r.body -= n
	if r.body == 0 {
		r.body = bodyTail
	}
	return n, nil
}

// fill 保证 r.msg 非空 —— 「WebSocket 消息 → 字节流」的那一步。
func (r *wsFrameReader) fill() error {
	for len(r.msg) == 0 {
		_, data, err := r.ws.ReadMessage()
		if err != nil {
			return err
		}
		r.msg = data
	}
	return nil
}

// readLine 读一行（到 \n 为止、含 \n）并追加到 dst，可跨多条 WS 消息；
// 头段总量（含未完成的行）受头段上限约束。空行判定由调用方做 —— 它才知道整行的起点。
func (r *wsFrameReader) readLine(dst []byte) ([]byte, error) {
	for {
		if err := r.fill(); err != nil {
			return nil, err
		}
		if i := bytes.IndexByte(r.msg, '\n'); i >= 0 {
			dst = append(dst, r.msg[:i+1]...)
			r.msg = r.msg[i+1:]
			if len(dst) > r.maxHeader {
				return nil, errFrameTooLarge
			}
			return dst, nil
		}
		// 本条消息里没有换行：整块并入，继续读下一条消息。
		dst = append(dst, r.msg...)
		r.msg = nil
		if len(dst) > r.maxHeader {
			return nil, errFrameTooLarge
		}
	}
}

// isBlankLine 判断一行是否为空行（LF 或 CRLF）。
func isBlankLine(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	return len(line) == 0
}

// push 把字节放进转发队列。调用时机保证 out 已排空或正在续接。
func (r *wsFrameReader) push(b []byte) {
	if r.off >= len(r.out) {
		r.out, r.off = r.out[:0], 0
	}
	r.out = append(r.out, b...)
}

// declaredContentLength 取头段里首个 content-length 的值。解析方式与 frame 包对齐：
// 精确头名、不去空白、十进制 uint32（解析失败时不在这里报错，交给库）。
func declaredContentLength(header []byte) (int, bool) {
	for _, line := range bytes.Split(header, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok || string(name) != frame.ContentLength {
			continue
		}
		n, err := strconv.ParseUint(string(value), 10, 32)
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}
