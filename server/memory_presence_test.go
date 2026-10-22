package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 内存实现的存活判定按条目自己的存活时刻算，与后台清扫无关 —— 清扫只负责回收内存。
func TestMemoryPresenceExpiresAfterTTL(t *testing.T) {
	m := NewMemoryPresenceManager()
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()

	if err := m.Add(ctx, "uid-1", "ref-a", 50*time.Millisecond); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(ctx, "uid-2", "ref-b", time.Hour); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if stats, err := m.PresenceStats(ctx); err != nil || stats.NumUsers != 2 {
		t.Fatalf("未到期时 stats = %+v (err %v), want NumUsers 2", stats, err)
	}

	time.Sleep(200 * time.Millisecond) // 远超 uid-1 的 50ms，远不到 uid-2 的 1h

	stats, err := m.PresenceStats(ctx)
	if err != nil {
		t.Fatalf("PresenceStats: %v", err)
	}
	if stats.NumUsers != 1 || stats.NumClients != 1 {
		t.Errorf("超期后 stats = %+v, want {NumUsers:1 NumClients:1}（只剩未超期的 uid-2）", stats)
	}

	members, err := m.Presence(ctx)
	if err != nil {
		t.Fatalf("Presence: %v", err)
	}
	if len(members) != 1 || members[0].Login != "uid-2" {
		t.Errorf("超期后 Presence() = %+v, want 只剩 uid-2", members)
	}
}

// 后台清扫把超期条目真正从内存里删掉（读侧过滤只是不显示它们）。
// 直接调 sweepExpired 而不是等 30s 的 ticker。
func TestMemoryPresenceSweepReclaimsExpired(t *testing.T) {
	m := NewMemoryPresenceManager()
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()

	if err := m.Add(ctx, "uid-1", "ref-a", 20*time.Millisecond); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := m.Add(ctx, "uid-2", "ref-b", time.Hour); err != nil {
		t.Fatalf("Add: %v", err)
	}

	time.Sleep(60 * time.Millisecond)
	m.sweepExpired()

	m.mu.RLock()
	left := len(m.conns)
	m.mu.RUnlock()
	if left != 1 {
		t.Errorf("清扫后剩余条目 = %d, want 1（只该回收超期的 uid-1）", left)
	}
}

// 端口要求实现并发安全：Add/Remove 在连接协程与续期协程上调用，读在业务协程上调用。
// 断言只让失败有个说法，真正的把关是 -race。
func TestMemoryPresenceConcurrentAccess(t *testing.T) {
	m := NewMemoryPresenceManager()
	t.Cleanup(func() { _ = m.Close() })
	ctx := context.Background()

	const workers = 8

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			ref := fmt.Sprintf("ref-%d", i)
			for j := 0; j < 50; j++ {
				_ = m.Add(ctx, "uid-1", ref, time.Minute)
				_, _ = m.PresenceStats(ctx)
				_, _ = m.Presence(ctx)
				if j%2 == 0 {
					_ = m.Remove(ctx, "uid-1", ref)
				}
			}
		}(i)
	}
	wg.Wait()

	// 每轮的最后一次操作都是 Add（j=49 为奇数时不 Remove），故 8 条连接都在。
	stats, err := m.PresenceStats(ctx)
	if err != nil {
		t.Fatalf("PresenceStats: %v", err)
	}
	if stats.NumUsers != 1 || stats.NumClients != workers {
		t.Errorf("并发跑完后 stats = %+v, want {NumUsers:1 NumClients:%d}", stats, workers)
	}
}
