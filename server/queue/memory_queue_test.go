package queue

import (
	"testing"

	"github.com/go-stomp/stomp/v3/frame"
)

// 内存存储按队列名各自成链，先入先出，空队列返回 nil。
func TestMemoryQueueStorage(t *testing.T) {
	mq := NewMemoryQueueStorage()
	mq.Start()

	f1 := frame.New(frame.MESSAGE,
		frame.Destination, "/queue/test",
		frame.MessageId, "msg-001",
		frame.Subscription, "1")
	f2 := frame.New(frame.MESSAGE,
		frame.Destination, "/queue/test",
		frame.MessageId, "msg-002",
		frame.Subscription, "1")
	f3 := frame.New(frame.MESSAGE,
		frame.Destination, "/queue/test2",
		frame.MessageId, "msg-003",
		frame.Subscription, "2")

	for _, f := range []*frame.Frame{f1, f2, f3} {
		if err := mq.Enqueue(f.Header.Get(frame.Destination), f); err != nil {
			t.Fatalf("Enqueue(%s): %v", f.Header.Get(frame.Destination), err)
		}
	}

	// 出队是顺序敏感的，所以下面的用例靠切片顺序模拟同一条队列上的连续消费。
	cases := []struct {
		name     string
		queue    string
		want     *frame.Frame
		wantDesc string
	}{
		{"另一条队列为空", "/queue/other-queue", nil, "nil"},
		{"test2 只有一条", "/queue/test2", f3, "msg-003"},
		{"test 先入先出", "/queue/test", f1, "msg-001"},
		{"test 第二条", "/queue/test", f2, "msg-002"},
		{"test 取空", "/queue/test", nil, "nil"},
		{"test2 取空", "/queue/test2", nil, "nil"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mq.Dequeue(tc.queue)
			if err != nil {
				t.Fatalf("Dequeue(%s) error = %v, want nil", tc.queue, err)
			}
			if got != tc.want {
				t.Errorf("Dequeue(%s) = %v, want %s", tc.queue, got, tc.wantDesc)
			}
		})
	}
}
