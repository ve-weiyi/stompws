package client

// reply.go 装「服务端回给客户端的帧」：RECEIPT 与 ERROR。两者共用「先取走 receipt 头」
// 这条规则，故同置一处（内部兼有 *Conn 与 *StompHubServer 的方法）。

import (
	"time"

	"github.com/go-stomp/stomp/v3/frame"
)

// SendError 按 STOMP 规范回一帧 ERROR（不关连接；要「回完即关」用 sendErrorAndClose）。
func (c *Conn) SendError(message string, receiptId string) {
	errFrame := frame.New(frame.ERROR, frame.Message, message)
	if receiptId != "" {
		errFrame.Header.Add(frame.ReceiptId, receiptId)
	}
	c.SendFrame(errFrame)
}

// sendError 是 SendError 的 error 版：内部调用方据此把错误文案集中声明。
func (c *Conn) sendError(err error, receiptId string) {
	c.SendError(err.Error(), receiptId)
}

// SendErrorAndClose 按规范回一帧 ERROR 后再关闭连接 —— 服务端无法处理某帧时一律如此。
// 规范允许客户端收不到最后一帧，
// 所以只留一个短暂的刷出窗口，不为送达拖延关闭。
func (c *Conn) SendErrorAndClose(err error, receiptId string) {
	c.sendError(err, receiptId)

	// 给 writeLoop 一点时间把 ERROR 发出去（规范允许丢，但能发就发）。
	select {
	case <-time.After(100 * time.Millisecond):
	case <-c.closeChan:
	}
	c.close()
}

// takeReceipt 取走帧上的 receipt 头，返回它的值与「这一帧是否要求回执」
// （头存在但值为空也算要求回执）。必须在**路由/投递之前**取走：hook 可以把
// 原始 SEND 帧直接交给中继，残留的 receipt 会跟着消息泄漏给订阅者。
func takeReceipt(f *frame.Frame) (string, bool) {
	receiptID, ok := f.Header.Contains(frame.Receipt)
	if !ok {
		return "", false
	}
	f.Header.Del(frame.Receipt)
	return receiptID, true
}

// sendReceipt 按帧上的 receipt 头回执，并把它摘掉：回执是发送方与服务器之间的约定，
// 摘掉之后剩下的头部才不会随 MESSAGE 传给订阅者。
func (c *Conn) sendReceipt(f *frame.Frame) {
	if receiptID, ok := takeReceipt(f); ok {
		c.sendReceiptID(receiptID)
	}
}

func (c *Conn) sendReceiptID(receiptID string) {
	c.SendFrame(frame.New(frame.RECEIPT, frame.ReceiptId, receiptID))
}
