package server

import (
	"context"
	"testing"
	"time"
)

// presenceManagers 列出要满足端口契约的全部实现：契约写一遍，实现加进来就自动被检查。
func presenceManagers(t *testing.T) map[string]PresenceManager {
	t.Helper()

	memory := NewMemoryPresenceManager()
	t.Cleanup(func() { _ = memory.Close() })

	return map[string]PresenceManager{"memory": memory}
}

// 在线状态的粒度是**连接**：同一 login 的多条连接各算一条，断开其一不该让用户离线；
// 而重复 Add 只是续期，不该多出一条。
func TestPresenceManagerCountsConnectionsNotUsers(t *testing.T) {
	for name, m := range presenceManagers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ttl := time.Minute

			// 同一用户两条连接（多标签页/多设备），其中一条再续期一次。
			for _, ref := range []string{"ref-a", "ref-b", "ref-a"} {
				if err := m.Add(ctx, "uid-1", ref, ttl); err != nil {
					t.Fatalf("Add(uid-1, %s): %v", ref, err)
				}
			}

			stats, err := m.PresenceStats(ctx)
			if err != nil {
				t.Fatalf("PresenceStats: %v", err)
			}
			if stats.NumUsers != 1 {
				t.Errorf("NumUsers = %d, want 1", stats.NumUsers)
			}
			if stats.NumClients != 2 {
				t.Errorf("NumClients = %d, want 2（重复 Add 是续期，不该新增）", stats.NumClients)
			}

			// 断开其一：用户还活着，只是少了一条连接。
			if err := m.Remove(ctx, "uid-1", "ref-a"); err != nil {
				t.Fatalf("Remove: %v", err)
			}

			stats, err = m.PresenceStats(ctx)
			if err != nil {
				t.Fatalf("PresenceStats: %v", err)
			}
			if stats.NumUsers != 1 || stats.NumClients != 1 {
				t.Errorf("断开两条连接之一后 stats = %+v, want {NumUsers:1 NumClients:1}", stats)
			}

			members, err := m.Presence(ctx)
			if err != nil {
				t.Fatalf("Presence: %v", err)
			}
			if len(members) != 1 || members[0].Login != "uid-1" || members[0].Ref != "ref-b" {
				t.Errorf("Presence() = %+v, want 只剩 uid-1/ref-b", members)
			}
		})
	}
}

// 摘除一条不存在的连接（重复断开、迟到的清理）不该报错，也不该动到别人。
func TestPresenceManagerRemoveUnknownIsNoop(t *testing.T) {
	for name, m := range presenceManagers(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			if stats, err := m.PresenceStats(ctx); err != nil || stats.NumClients != 0 {
				t.Fatalf("空名单的 stats = %+v (err %v), want 零值", stats, err)
			}
			if members, err := m.Presence(ctx); err != nil || len(members) != 0 {
				t.Fatalf("空名单的 Presence() = %+v (err %v), want 空", members, err)
			}

			if err := m.Add(ctx, "uid-1", "ref-a", time.Minute); err != nil {
				t.Fatalf("Add: %v", err)
			}
			if err := m.Remove(ctx, "uid-1", "ref-a"); err != nil {
				t.Fatalf("Remove 已存在的连接: %v", err)
			}
			if err := m.Remove(ctx, "uid-1", "ref-a"); err != nil {
				t.Errorf("重复 Remove = %v, want nil", err)
			}
		})
	}
}
