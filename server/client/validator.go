package client

import (
	"regexp"

	"github.com/go-stomp/stomp/v3"
	"github.com/go-stomp/stomp/v3/frame"
)

var destPattern = regexp.MustCompile(`^/[a-zA-Z0-9/_-]+$`)

func validateDestination(dest string) bool {
	return destPattern.MatchString(dest)
}

// validAckMode 判断 SUBSCRIBE 的 ack 取值；空值等同 auto（handleSubscribe 也这么补）。
//
// 必须在订阅时就拦住非法值：它会存进订阅，而 IsAckedBy 只认这三种模式 ——
// 非法值让任何 ACK 都匹配不上，帧卡在 subList 里、队列订阅再也挂不回去，
// 客户端却会因为收到 RECEIPT 而以为一切正常。
func validAckMode(ack string) bool {
	switch ack {
	case "", frame.AckAuto, frame.AckClient, frame.AckClientIndividual:
		return true
	default:
		return false
	}
}

type validator struct {
	version stomp.Version
}

func newValidator(version stomp.Version) stomp.Validator {
	return &validator{version: version}
}

func (v *validator) Validate(f *frame.Frame) error {
	switch f.Command {
	case frame.CONNECT, frame.STOMP:
		if _, ok := f.Header.Contains(frame.Receipt); ok {
			return errReceiptInConnect
		}
		if v.version != stomp.V10 {
			if _, ok := f.Header.Contains(frame.AcceptVersion); !ok {
				return errMissingHeader(frame.AcceptVersion)
			}
			// 1.1/1.2 的 CONNECT 里 host 是 REQUIRED，按规范强制。stompjs 不会自动加它，
			// 消费方必须在 connectHeaders 里显式给 —— 这是客户端的问题，不是放宽服务端的理由。
			if _, ok := f.Header.Contains(frame.Host); !ok {
				return errMissingHeader(frame.Host)
			}
		}
	case frame.SEND:
		if _, ok := f.Header.Contains(frame.Destination); !ok {
			return errMissingHeader(frame.Destination)
		}
		if !validateDestination(f.Header.Get(frame.Destination)) {
			return errInvalidDestination
		}
	case frame.SUBSCRIBE:
		if _, ok := f.Header.Contains(frame.Destination); !ok {
			return errMissingHeader(frame.Destination)
		}
		if !validateDestination(f.Header.Get(frame.Destination)) {
			return errInvalidDestination
		}
		if _, ok := f.Header.Contains(frame.Id); !ok {
			return errMissingHeader(frame.Id)
		}
		if ack, ok := f.Header.Contains(frame.Ack); ok && !validAckMode(ack) {
			return errInvalidAckMode(ack)
		}
	case frame.UNSUBSCRIBE:
		if _, ok := f.Header.Contains(frame.Id); !ok {
			return errMissingHeader(frame.Id)
		}
	case frame.ACK:
		if v.version == stomp.V12 {
			if _, ok := f.Header.Contains(frame.Id); !ok {
				return errMissingHeader(frame.Id)
			}
		} else if _, ok := f.Header.Contains(frame.MessageId); !ok {
			if _, ok := f.Header.Contains(frame.Ack); !ok {
				return errMissingHeader("message-id or ack")
			}
		}
	case frame.NACK:
		if v.version == stomp.V10 {
			return errNackNotSupported
		}
		if v.version == stomp.V12 {
			if _, ok := f.Header.Contains(frame.Id); !ok {
				return errMissingHeader(frame.Id)
			}
		} else if _, ok := f.Header.Contains(frame.MessageId); !ok {
			if _, ok := f.Header.Contains(frame.Ack); !ok {
				return errMissingHeader("message-id or ack")
			}
		}
	case frame.BEGIN, frame.COMMIT, frame.ABORT:
		if _, ok := f.Header.Contains(frame.Transaction); !ok {
			return errMissingHeader(frame.Transaction)
		}
	}
	return nil
}
