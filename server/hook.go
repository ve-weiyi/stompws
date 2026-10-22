package server

import (
	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
)

// EventHook defines callbacks for STOMP events
//
// 第一个参数 hub 就是回调的上下文：钩子需要什么，从它取（如 hub.Presence()），
// 不必再当成构造参数注入一遍 —— hub 已持有的东西重复注入，只会多出
// 「hub 用一个、钩子用另一个」这种查不出来的错配。
type EventHook interface {
	// OnConnect is called after a client successfully connects
	// server parameter allows sending messages back to client
	OnConnect(server *StompHubServer, c *client.Conn)

	// OnDisconnect is called when a client disconnects
	OnDisconnect(server *StompHubServer, c *client.Conn)

	// OnSubscribe is called after a client subscribes to a destination
	// server parameter allows sending welcome messages
	OnSubscribe(server *StompHubServer, c *client.Conn, destination string, subscriptionId string)

	// OnUnsubscribe is called after a client unsubscribes from a destination
	OnUnsubscribe(server *StompHubServer, c *client.Conn, destination string, subscriptionId string)

	// OnSend is called when a client sends a message
	// Return false to prevent the message from being sent
	OnSend(server *StompHubServer, c *client.Conn, message *frame.Frame) bool

	// OnMessage is called before a message is delivered to a client
	// Return false to prevent the message from being delivered
	//
	// Deprecated: 本实现不调用它（投递在中继协程上，过滤请放在 OnSend）。
	OnMessage(c *client.Conn, destination string, message *frame.Frame) bool

	// OnAck is called when a client acknowledges a message
	OnAck(c *client.Conn, messageId string)

	// OnNack is called when a client negatively acknowledges a message
	OnNack(c *client.Conn, messageId string)

	// OnError is called when an error occurs
	//
	// Deprecated: 本实现不调用它（错误以 ERROR 帧回给客户端并记录日志）。
	OnError(c *client.Conn, err error)
}

// DefaultEventHook provides default no-op implementations
type DefaultEventHook struct{}

func NewDefaultEventHook() *DefaultEventHook {
	return &DefaultEventHook{}
}

func (h *DefaultEventHook) OnConnect(server *StompHubServer, c *client.Conn)    {}
func (h *DefaultEventHook) OnDisconnect(server *StompHubServer, c *client.Conn) {}
func (h *DefaultEventHook) OnSubscribe(server *StompHubServer, c *client.Conn, destination string, subscriptionId string) {
}
func (h *DefaultEventHook) OnUnsubscribe(server *StompHubServer, c *client.Conn, destination string, subscriptionId string) {
}
func (h *DefaultEventHook) OnSend(server *StompHubServer, c *client.Conn, message *frame.Frame) bool {
	return true
}
func (h *DefaultEventHook) OnMessage(c *client.Conn, destination string, message *frame.Frame) bool {
	return true
}
func (h *DefaultEventHook) OnAck(c *client.Conn, messageId string)  {}
func (h *DefaultEventHook) OnNack(c *client.Conn, messageId string) {}
func (h *DefaultEventHook) OnError(c *client.Conn, err error)       {}
