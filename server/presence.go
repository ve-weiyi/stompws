package server

import (
	"context"
	"time"
)

// presence.go 装「谁在线」的存储端口：hub 往里报连接的来去，业务从里查名单与计数。
// 在线状态的**通知**不走这里 —— 那是事件钩子的事（见 hook.go）。

// PresenceMember 是一条在线连接。粒度是连接而非用户：同一 login 可以有多条
// （多标签页、多设备），断开其一不该把用户判为离线。
type PresenceMember struct {
	Login        string `json:"user_id"` // 用户标识
	Ref          string // 连接标识（本仓取 clientId），用来区分同一 login 的多条连接
	LastActiveAt int64  `json:"last_active_at"` // 最近一次续期的时刻（unix 毫秒）
}

// PresenceStats 是在线状态的两种口径。
type PresenceStats struct {
	NumUsers   int // 唯一 login 数 —— 产品口径的「在线人数」
	NumClients int // 连接数 —— 多端登录会各算一个
}

// PresenceManager 是在线状态的存储端口，默认实现是 MemoryPresenceManager。
//
// 并发契约：Add/Remove 在连接的 processLoop 与 hub 的续期协程上调用，
// Presence/PresenceStats 在任意业务协程上调用 —— 实现必须并发安全；
// Close 由 hub 的 Shutdown 调用一次。
//
// 实现只管存储：签名里没有 hub，也就没有回拿 hub 去广播的通道 —— 通知归钩子。
type PresenceManager interface {
	// Add 登记一条连接并续期。同一 (login, ref) 重复调用必须幂等 —— 续期走的就是这条路。
	Add(ctx context.Context, login, ref string, ttl time.Duration) error

	// Remove 摘除一条连接。该 login 还有其他 ref 时，用户仍在名单里。
	Remove(ctx context.Context, login, ref string) error

	// PresenceStats 返回两种口径的计数 —— 名单可能很大，计数不该靠全量拉取。
	PresenceStats(ctx context.Context) (PresenceStats, error)

	// Presence 列出当前在线的连接。
	Presence(ctx context.Context) ([]PresenceMember, error)

	// Close 释放自身资源（后台协程、连接）。由 hub 的 Shutdown 调用。
	Close() error
}
