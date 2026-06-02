package client

import (
	"context"
	"sync"
	"time"
)

// MemoryOnlineTracker is the default in-memory OnlineTracker implementation.
// It keeps online users in a local map with a periodic cleanup goroutine.
type MemoryOnlineTracker struct {
	mu            sync.RWMutex
	users         map[string]int64 // login -> lastActiveAt (unix milli)
	cleanupCtx    context.Context
	cleanupCancel context.CancelFunc
}

func NewMemoryOnlineTracker() *MemoryOnlineTracker {
	ctx, cancel := context.WithCancel(context.Background())
	t := &MemoryOnlineTracker{
		users:         make(map[string]int64),
		cleanupCtx:    ctx,
		cleanupCancel: cancel,
	}
	go t.startCleanup()
	return t
}

func (t *MemoryOnlineTracker) SetHub(hub *StompHubServer) {}

func (t *MemoryOnlineTracker) OnConnect(login string) {
	t.mu.Lock()
	t.users[login] = time.Now().UnixMilli()
	t.mu.Unlock()
}

func (t *MemoryOnlineTracker) OnDisconnect(login string) {
	t.mu.Lock()
	delete(t.users, login)
	t.mu.Unlock()
}

func (t *MemoryOnlineTracker) OnActive(login string) {
	t.mu.Lock()
	t.users[login] = time.Now().UnixMilli()
	t.mu.Unlock()
}

func (t *MemoryOnlineTracker) GetOnlineCount(ctx context.Context) (int64, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return int64(len(t.users)), nil
}

func (t *MemoryOnlineTracker) GetOnlineUsers(ctx context.Context, maxAgeSec int64) ([]*OnlineUser, error) {
	if maxAgeSec <= 0 {
		maxAgeSec = 60
	}
	threshold := time.Now().Add(-time.Duration(maxAgeSec) * time.Second).UnixMilli()

	t.mu.RLock()
	defer t.mu.RUnlock()

	users := make([]*OnlineUser, 0, len(t.users))
	for login, lastActive := range t.users {
		if lastActive >= threshold {
			users = append(users, &OnlineUser{UserId: login, LastActiveAt: lastActive})
		}
	}
	return users, nil
}

func (t *MemoryOnlineTracker) Close() error {
	t.cleanupCancel()
	return nil
}

func (t *MemoryOnlineTracker) startCleanup() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			threshold := time.Now().Add(-60 * time.Second).UnixMilli()
			t.mu.Lock()
			for login, lastActive := range t.users {
				if lastActive < threshold {
					delete(t.users, login)
				}
			}
			t.mu.Unlock()
		case <-t.cleanupCtx.Done():
			return
		}
	}
}
