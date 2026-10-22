package server

import (
	"context"
	"sync"
	"time"
)

// memoryPresenceSweepInterval 是回收超期条目的节奏。它只影响内存占用 ——
// 读侧一律按条目的存活时刻过滤，正确性不依赖清扫及不及时。
const memoryPresenceSweepInterval = 30 * time.Second

// MemoryPresenceManager 把在线状态放在本节点内存里，按 (login, ref) 记连接。
// 它只看得见本节点的连接；要多节点共享一份名单，用共享存储的实现。
type MemoryPresenceManager struct {
	mu    sync.RWMutex
	conns map[presenceKey]presenceEntry

	ctx    context.Context
	cancel context.CancelFunc
}

// presenceKey 是一条连接的标识：同一 login 的多条连接靠 ref 区分。
type presenceKey struct {
	login string
	ref   string
}

type presenceEntry struct {
	lastActiveAt int64 // 最近一次 Add 的时刻（unix 毫秒），即对外的 LastActiveAt
	expiresAt    int64 // 存活到此刻（unix 毫秒）：该次 Add 的时刻 + 传入的 ttl
}

func NewMemoryPresenceManager() *MemoryPresenceManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &MemoryPresenceManager{
		conns:  make(map[presenceKey]presenceEntry),
		ctx:    ctx,
		cancel: cancel,
	}
	go m.sweepLoop()
	return m
}

// Add 登记并续期：同一 (login, ref) 重复调用只刷新时刻，不会多出一条。
func (m *MemoryPresenceManager) Add(_ context.Context, login, ref string, ttl time.Duration) error {
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.conns[presenceKey{login: login, ref: ref}] = presenceEntry{
		lastActiveAt: now.UnixMilli(),
		expiresAt:    now.Add(ttl).UnixMilli(),
	}
	return nil
}

// Remove 摘除一条连接；同 login 的其他 ref 不受影响。
func (m *MemoryPresenceManager) Remove(_ context.Context, login, ref string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.conns, presenceKey{login: login, ref: ref})
	return nil
}

func (m *MemoryPresenceManager) PresenceStats(context.Context) (PresenceStats, error) {
	now := time.Now().UnixMilli()

	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := PresenceStats{}
	users := make(map[string]struct{})
	for key, e := range m.conns {
		if e.expiresAt < now {
			continue
		}
		stats.NumClients++
		users[key.login] = struct{}{}
	}
	stats.NumUsers = len(users)
	return stats, nil
}

func (m *MemoryPresenceManager) Presence(context.Context) ([]PresenceMember, error) {
	now := time.Now().UnixMilli()

	m.mu.RLock()
	defer m.mu.RUnlock()

	members := make([]PresenceMember, 0, len(m.conns))
	for key, e := range m.conns {
		if e.expiresAt < now {
			continue
		}
		members = append(members, PresenceMember{
			Login:        key.login,
			Ref:          key.ref,
			LastActiveAt: e.lastActiveAt,
		})
	}
	return members, nil
}

func (m *MemoryPresenceManager) Close() error {
	m.cancel()
	return nil
}

func (m *MemoryPresenceManager) sweepLoop() {
	ticker := time.NewTicker(memoryPresenceSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.sweepExpired()
		case <-m.ctx.Done():
			return
		}
	}
}

// sweepExpired 回收超期条目，只影响内存占用。
func (m *MemoryPresenceManager) sweepExpired() {
	now := time.Now().UnixMilli()

	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.conns {
		if e.expiresAt < now {
			delete(m.conns, key)
		}
	}
}

var _ PresenceManager = (*MemoryPresenceManager)(nil)
