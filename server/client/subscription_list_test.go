package client

import (
	"testing"

	"github.com/go-stomp/stomp/v3/frame"
)

// SubscriptionList 的单测：入列出列顺序、移除，以及按 ack 模式判定的累积/单独确认。
// 这里只钉这个数据结构本身。

// ackSub 造一个只用于确认判定的订阅（不挂连接，列表不需要它）。
func ackSub(dest, id, ack string, msgId uint64) *Subscription {
	return &Subscription{dest: dest, id: id, ack: ack, msgId: msgId}
}

// Get 取自链首并移除；再次入列排到队尾。
func TestSubscriptionListAddAndGet(t *testing.T) {
	sub1 := ackSub("/queue/a", "1", frame.AckClient, 1)
	sub2 := ackSub("/queue/a", "2", frame.AckClient, 2)
	sub3 := ackSub("/queue/a", "3", frame.AckClient, 3)

	sl := NewSubscriptionList()
	sl.Add(sub1)
	sl.Add(sub2)
	sl.Add(sub3)

	for _, want := range []*Subscription{sub1, sub2, sub3} {
		if got := sl.Get(); got != want {
			t.Fatalf("Get() = %v, want %v（先入先出）", got, want)
		}
	}

	// 已取出的订阅可以再次入列，并排到队尾。
	sl.Add(sub1)
	if got := sl.Get(); got != sub1 {
		t.Errorf("重新入列后 Get() = %v, want %v", got, sub1)
	}
	if got := sl.Get(); got != nil {
		t.Errorf("取空后 Get() = %v, want nil", got)
	}
}

// Remove 按对象移除，不在列表里的对象是空操作。
func TestSubscriptionListRemove(t *testing.T) {
	sub1 := ackSub("/queue/a", "1", frame.AckClient, 1)
	sub2 := ackSub("/queue/a", "2", frame.AckClient, 2)
	sub3 := ackSub("/queue/a", "3", frame.AckClient, 3)
	other := ackSub("/queue/a", "9", frame.AckClient, 9)

	sl := NewSubscriptionList()
	sl.Add(sub1)
	sl.Add(sub2)
	sl.Add(sub3)

	sl.Remove(other) // 不在列表里：不该有副作用
	sl.Remove(sub2)

	if got := sl.Get(); got != sub1 {
		t.Errorf("Get() = %v, want %v", got, sub1)
	}
	if got := sl.Get(); got != sub3 {
		t.Errorf("Get() = %v, want %v（sub2 已被移除）", got, sub3)
	}
	if got := sl.Get(); got != nil {
		t.Errorf("Get() = %v, want nil", got)
	}
}

// Ack：ack=client 是累积确认（确认某条即确认它之前的全部），client-individual 只认自己那条。
func TestSubscriptionListAck(t *testing.T) {
	cumulative := ackSub("/queue/d1", "1", frame.AckClient, 101)
	individual := ackSub("/queue/d2", "2", frame.AckClientIndividual, 102)
	exact := ackSub("/queue/d3", "3", frame.AckClient, 103)
	later := ackSub("/queue/d4", "4", frame.AckClient, 104)

	sl := NewSubscriptionList()
	for _, sub := range []*Subscription{cumulative, individual, exact, later} {
		sl.Add(sub)
	}

	var acked []*Subscription
	sl.Ack(103, func(sub *Subscription) { acked = append(acked, sub) })

	// 101、103 被累积确认（101 <= 103）；102 是 individual 且 id 不等，104 还没到。
	if len(acked) != 2 || acked[0] != cumulative || acked[1] != exact {
		t.Fatalf("Ack(103) 确认了 %v, want [%v %v]", acked, cumulative, exact)
	}

	for _, want := range []*Subscription{individual, later} {
		if got := sl.Get(); got != want {
			t.Errorf("Get() = %v, want %v（未被确认的应当还在列表里）", got, want)
		}
	}
	if got := sl.Get(); got != nil {
		t.Errorf("Get() = %v, want nil", got)
	}
}

// Nack 只认自己那一条（本实现的理解：NACK 都是单独的）。
func TestSubscriptionListNack(t *testing.T) {
	first := ackSub("/queue/d1", "1", frame.AckClient, 101)
	second := ackSub("/queue/d2", "2", frame.AckClient, 102)
	third := ackSub("/queue/d3", "3", frame.AckClient, 103)

	sl := NewSubscriptionList()
	for _, sub := range []*Subscription{first, second, third} {
		sl.Add(sub)
	}

	var nacked []*Subscription
	sl.Nack(102, func(sub *Subscription) { nacked = append(nacked, sub) })

	if len(nacked) != 1 || nacked[0] != second {
		t.Fatalf("Nack(102) 拒绝了 %v, want [%v]", nacked, second)
	}
	// 累积确认不适用于 NACK：更早的 first 不该被牵走。
	for _, want := range []*Subscription{first, third} {
		if got := sl.Get(); got != want {
			t.Errorf("Get() = %v, want %v", got, want)
		}
	}
}

// Ack/Nack 摘掉订阅时必须一并清掉 subList 回指针：否则确认之后重新订阅
// 同一个队列、再次 Add 时会撞上「已在列表中」的 panic。
func TestSubscriptionListAckAllowsResubscribe(t *testing.T) {
	sub := ackSub("/queue/a", "1", frame.AckClient, 101)

	sl := NewSubscriptionList()
	sl.Add(sub)

	sl.Ack(101, func(s *Subscription) {})
	if sub.subList != nil {
		t.Errorf("Ack 后 subList = %v, want nil", sub.subList)
	}

	// 重新订阅（再次入列）不得 panic。
	sl.Add(sub)

	sl.Nack(101, func(s *Subscription) {})
	if sub.subList != nil {
		t.Errorf("Nack 后 subList = %v, want nil", sub.subList)
	}
	sl.Add(sub)
}

// 重复入列必须当场 panic：静默接受会让队列以为有两个消费者，
// 把下一条消息也派给同一个订阅（它的上一帧还没确认）。
// 队列的等待消费者表与本表同类型，这条契约对两者都成立。
func TestSubscriptionListAddTwicePanics(t *testing.T) {
	sub := ackSub("/queue/jobs", "1", frame.AckAuto, 1)

	sl := NewSubscriptionList()
	sl.Add(sub)

	defer func() {
		if recover() == nil {
			t.Error("同一个订阅重复 Add 没有 panic")
		}
	}()
	sl.Add(sub)
}
