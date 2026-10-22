package server

import (
	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
)

// NoAuthenticator 放行所有连接：login 取 CONNECT 的 login 头，缺省用对端地址。
type NoAuthenticator struct{}

func NewNoAuthenticator() *NoAuthenticator {
	return &NoAuthenticator{}
}

func (a *NoAuthenticator) Authenticate(c *client.Conn, f *frame.Frame) (string, string, error) {
	login := f.Header.Get(frame.Login)
	clientId := c.GetIpAddress()

	if login == "" {
		login = clientId
	}
	return clientId, login, nil
}
