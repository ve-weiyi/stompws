package client

import "fmt"

// Error messages following STOMP protocol conventions
type errorMessage string

func (e errorMessage) Error() string {
	return string(e)
}

// Standard STOMP error messages
const (
	errNotConnected         = errorMessage("expected CONNECT or STOMP frame")
	errAlreadyConnected     = errorMessage("already connected")
	errUnknownCommand       = errorMessage("unknown command")
	errReceiptInConnect     = errorMessage("receipt header not allowed in CONNECT/STOMP frame")
	errSubscriptionExists   = errorMessage("subscription already exists")
	errSubscriptionNotFound = errorMessage("subscription not found")
	errTransactionExists    = errorMessage("transaction already exists")
	errTransactionNotFound  = errorMessage("unknown transaction")
	errInvalidHeartBeat     = errorMessage("invalid heart-beat format")
	errHeartBeatTooLarge    = errorMessage("heart-beat value too large")
	errUnsupportedVersion   = errorMessage("unsupported protocol version")
	errAuthenticationFailed = errorMessage("authentication failed")
	errInvalidDestination   = errorMessage("invalid destination")
	errNackNotSupported     = errorMessage("NACK not supported in STOMP 1.0")
	errFrameTooLarge        = errorMessage("frame too large")
	errServerShuttingDown   = errorMessage("server shutting down")
)

// 带参数的错误文案也集中在这里，免得同一个说法散成多份字面量。
func errMissingHeader(name string) error {
	return fmt.Errorf("missing required header: %s", name)
}

func errFrameParse(cause error) error {
	return fmt.Errorf("frame parse error: %v", cause)
}

func errNonNumericAckID(ackID string) error {
	return fmt.Errorf("ACK/NACK with non-numeric id: %s", ackID)
}

func errInvalidAckMode(ack string) error {
	return fmt.Errorf("invalid ack mode: %q (want auto, client or client-individual)", ack)
}
