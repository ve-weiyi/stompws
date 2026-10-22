package server

import (
	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
)

// RequestOp 是中继请求的 opcode。
type RequestOp int

// 中继请求的合法 opcode。
const (
	SubscribeOp   RequestOp = iota // 订阅已建立，请挂到目的地
	UnsubscribeOp                  // 订阅已撤销，请从目的地摘掉
	EnqueueOp                      // 投递一条消息
	RequeueOp                      // 重新入队一条未能送达的消息
)

func (r RequestOp) String() string {
	switch r {
	case SubscribeOp:
		return "Subscribe"
	case UnsubscribeOp:
		return "Unsubscribe"
	case EnqueueOp:
		return "Enqueue"
	case RequeueOp:
		return "Requeue"
	default:
		return "Unknown"
	}
}

// Request 是各协程提交给中继循环的请求。中继状态由那一个 goroutine 独占，
// 因此 server/topic 与 server/queue 不需要任何锁。
type Request struct {
	Op    RequestOp            // opcode
	Sub   *client.Subscription // SubscribeOp、UnsubscribeOp
	Frame *frame.Frame         // EnqueueOp、RequeueOp

	// Err 非 nil 表示调用方等结果（中继侧非阻塞回值）；通道必须带缓冲，
	// 否则调用方放弃等待时中继会卡死。nil 表示 fire-and-forget。
	Err chan error
}

// reply 把处理结果交给等待中的调用方。必须非阻塞：调用方可能已因连接关闭而放弃等待，
// 阻塞式发送会让中继 goroutine 永久卡住、整个中继停摆。
func (r Request) reply(err error) {
	if r.Err == nil {
		return
	}
	select {
	case r.Err <- err:
	default:
	}
}
