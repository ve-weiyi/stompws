package client

import "context"

// OnlineUser represents an online user with last active time.
type OnlineUser struct {
	UserId       string `json:"user_id"`
	LastActiveAt int64  `json:"last_active_at"` // milliseconds timestamp
}

// OnlineTracker is the persistence backend for online user tracking.
// StompHubServer owns the connection lifecycle and sync loop; the tracker
// handles storage and queries.
type OnlineTracker interface {
	SetHub(hub *StompHubServer) // called by StompHubServer after construction
	OnConnect(login string)
	OnDisconnect(login string)
	OnActive(login string)
	GetOnlineUsers(ctx context.Context, maxAgeSec int64) ([]*OnlineUser, error)
	GetOnlineCount(ctx context.Context) (int64, error)
	Close() error
}
