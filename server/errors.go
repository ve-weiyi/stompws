package server

import "fmt"

// 服务端与中继自身的错误文案。
//
// 两边**各自持有、互不引用** —— 连接层不该认识服务端的内部常量，反之亦然。
// 连接层不该认识服务端的内部常量，服务端也不该反向依赖它的内部符号。
// 因此少数文案（"invalid destination" 一类的协议措辞）在两边都存在，这是有意的。

type stompError string

func (e stompError) Error() string {
	return string(e)
}

const (
	errTooManyDestinations = stompError("too many destinations")
	errInvalidDestination  = stompError("invalid destination")
	errServerShuttingDown  = stompError("server shutting down")
)

func errInvalidDestinationFor(dest string) error {
	return fmt.Errorf("invalid destination: %s", dest)
}
