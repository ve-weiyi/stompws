package topic

import (
	"testing"

	"github.com/go-stomp/stomp/v3/frame"
)

func TestTopic(t *testing.T) {
	t.Run("没有订阅者时广播不出错", func(t *testing.T) {
		topic := newTopic("destination")

		f := frame.New(frame.MESSAGE,
			frame.Destination, "destination")

		topic.Enqueue(f)
	})

	t.Run("一个订阅者直接收到原帧", func(t *testing.T) {
		sub := &fakeSubscription{}

		topic := newTopic("destination")
		topic.Subscribe(sub)

		f := frame.New(frame.MESSAGE,
			frame.Destination, "destination")

		topic.Enqueue(f)

		if len(sub.Frames) != 1 {
			t.Fatalf("收到 %d 个帧, want 1", len(sub.Frames))
		}
		if sub.Frames[0] != f {
			t.Errorf("收到的帧 = %p, want 原帧 %p", sub.Frames[0], f)
		}
	})

	t.Run("两个订阅者中最后一个收原帧其余收副本", func(t *testing.T) {
		sub1 := &fakeSubscription{}
		sub2 := &fakeSubscription{}

		topic := newTopic("destination")
		topic.Subscribe(sub1)
		topic.Subscribe(sub2)

		f := frame.New(frame.MESSAGE,
			frame.Destination, "destination",
			"xxx", "yyy")

		topic.Enqueue(f)

		if len(sub1.Frames) != 1 {
			t.Fatalf("sub1 收到 %d 个帧, want 1", len(sub1.Frames))
		}
		if len(sub2.Frames) != 1 {
			t.Fatalf("sub2 收到 %d 个帧, want 1", len(sub2.Frames))
		}
		if sub1.Frames[0] == f {
			t.Error("sub1 收到的是原帧, want 一份副本")
		}
		if sub2.Frames[0] != f {
			t.Errorf("sub2 收到的帧 = %p, want 原帧 %p", sub2.Frames[0], f)
		}
	})
}

type fakeSubscription struct {
	// frames received by the subscription
	Frames []*frame.Frame
}

func (s *fakeSubscription) SendTopicFrame(f *frame.Frame) {
	s.Frames = append(s.Frames, f)
}
