package client

// config.go 装「连接层需要从服务端拿到的东西」：由 NewConn 注入，
// 连接层因此不认识服务端侧的类型。

import (
	"time"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/logws"
)

// Authenticator 在 CONNECT 时校验身份，返回连接标识与 login。
// 实现由服务端注入 —— 连接层只认这个接口，不认具体实现。
type Authenticator interface {
	Authenticate(c *Conn, f *frame.Frame) (clientId string, login string, err error)
}

// Config 是服务端注入的连接级参数。
type Config struct {
	Log            logws.Logger
	Authenticator  Authenticator // 校验客户端登录/口令的一方；零值 = 放行所有（见 allowAllAuthenticator）
	ConnectTimeout time.Duration // 连上但一直不发 CONNECT 的容忍时长；<= 0 取 DefaultConnectTimeout
	MinHeartBeat   time.Duration // 协商心跳间隔的下限；<= 0 取 DefaultMinHeartBeat
}

// 默认值。服务端与直接构造连接的一方共用这一份 —— 两处各写一个数就会悄悄走偏。
const (
	DefaultConnectTimeout = 10 * time.Second
	DefaultMinHeartBeat   = 30 * time.Second
)

// withDefaults 补上没填的字段并返回副本；不改调用方传进来的值。
func (c Config) withDefaults() Config {
	if c.Log == nil {
		c.Log = logws.NewDefaultLogger()
	}
	if c.Authenticator == nil {
		c.Authenticator = allowAllAuthenticator{}
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.MinHeartBeat <= 0 {
		c.MinHeartBeat = DefaultMinHeartBeat
	}
	return c
}

// allowAllAuthenticator 是 Authenticator 为零值时的兜底：**不做任何校验**，
// login 取 CONNECT 的 login 头、缺省用对端地址。
//
// 它只用来兜底，不在服务端的装配面上 —— 那边要「不认证」请显式传 server 的
// NoAuthenticator，好让「这个部署不做认证」在代码里看得见。
type allowAllAuthenticator struct{}

func (allowAllAuthenticator) Authenticate(c *Conn, f *frame.Frame) (string, string, error) {
	login := f.Header.Get(frame.Login)
	clientId := c.GetIpAddress()

	if login == "" {
		login = clientId
	}
	return clientId, login, nil
}
