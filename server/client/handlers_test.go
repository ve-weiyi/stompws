package client

// handlers.go 的白盒测试：断开清理的退回契约、ACK/NACK 的 id 解析。

import (
	"testing"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/logws"
)

// 断开清理负责「已写出、未确认」的队列帧（挂在 subList 上）；未写出的那些由
// writeLoop 退出时退回 —— 连接侧读 sub.frame 会与中继协程竞争。
func TestCleanupRequeuesUnackedFrame(t *testing.T) {
	b := newFakeHub()

	c := NewConn(Config{Log: &logws.NoOpLogger{}}, nil, b)
	c.id = "test-client"
	c.login = "test-user"
	c.connected = true

	f := frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs", frame.MessageId, "1")
	sub := NewSubscription(c, "/queue/jobs", "sub-1", frame.AckClient)
	sub.frame = f
	c.subscriptions["sub-1"] = sub

	c.subListMu.Lock()
	c.subList.Add(sub) // 已写出、等待确认
	c.subListMu.Unlock()

	c.cleanup()

	// 断言的是「连接层把未确认帧交给中继退回」这条契约。
	got := b.tookRequeue()
	if len(got) != 1 || got[0] != f {
		t.Errorf("退回的帧 = %v, want [%v]", got, f)
	}
}

// ACK/NACK 的 id 必须能解析成数字：解析不出来要交出来（吞掉会得到 0，
// 而 IsAckedBy(0) 恒为 false，ACK 会被静默丢弃）。
func TestStringToUint64(t *testing.T) {
	if got, err := stringToUint64("123"); err != nil || got != 123 {
		t.Errorf("stringToUint64(123) = %d, %v, want 123, nil", got, err)
	}
	if got, err := stringToUint64("0"); err != nil || got != 0 {
		t.Errorf("stringToUint64(0) = %d, %v, want 0, nil", got, err)
	}
	for _, bad := range []string{"", "abc", "-1", "1.5", "0x10"} {
		if _, err := stringToUint64(bad); err == nil {
			t.Errorf("stringToUint64(%q) 应当报错", bad)
		}
	}
}
