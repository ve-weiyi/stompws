// requestProcessor 的请求语义：提交、关停、panic 兜底、目的地数量上限。
package server

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/server/client"
)

// 队列订阅失败时 SubscribeToDestination 必须把错误交出来，
// 否则 handleSubscribe 会照发 RECEIPT，向客户端谎报订阅已建立。
func TestSubscribeToDestinationPropagatesQueueError(t *testing.T) {
	wantErr := errors.New("storage unavailable")

	s := newTestHub(t, WithQueueStorage(&fakeStorage{dequeueErr: wantErr}))
	sub := client.NewSubscription(client.NewConn(client.Config{}, nil, s), "/queue/orders", "sub-1", frame.AckAuto)

	err := s.SubscribeToDestination(sub)
	if !errors.Is(err, wantErr) {
		t.Fatalf("SubscribeToDestination() = %v, want %v", err, wantErr)
	}
}

// ──────────────── 阶段 6：关停语义 ────────────────

// 关停之后提交必须明确失败，不能谎报成功 —— 返回 nil 的语义是「中继会处理它」，
// 而已经停掉的中继不会再处理任何请求。
func TestSubmitAfterStopFails(t *testing.T) {
	s := newTestHub(t)

	s.proc.stop()
	<-s.proc.stopped

	for i := 0; i < 100; i++ {
		err := s.proc.submit(Request{
			Op:    RequeueOp,
			Frame: frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs"),
		})
		if !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("第 %d 次提交返回 %v, want %v", i+1, err, ErrBrokerClosed)
		}
	}
}

// 关停时，已经受理（submit 返回过 nil）的请求必须被处理完，而不是静默丢弃 ——
// 否则「退回队首」那套在持久化 storage 下就成了真的丢消息。
func TestShutdownProcessesAcceptedRequests(t *testing.T) {
	gate := make(chan struct{})
	storage := &fakeStorage{gate: gate}
	s := newTestHub(t, WithQueueStorage(storage))

	f1 := frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs")
	f2 := frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs")

	// f1 会把中继卡在处理中（storage 在 gate 关闭前不放行）。
	if err := s.proc.submit(Request{Op: RequeueOp, Frame: f1}); err != nil {
		t.Fatalf("submit f1: %v", err)
	}
	// f2 只能等在通道里 —— 它的 submit 已经返回 nil，等于中继答应了要处理。
	if err := s.proc.submit(Request{Op: RequeueOp, Frame: f2}); err != nil {
		t.Fatalf("submit f2: %v", err)
	}

	s.proc.stop()
	close(gate)
	<-s.proc.stopped

	if got := storage.received(); len(got) != 2 {
		t.Fatalf("关停后只处理了 %d 条，want 2", len(got))
	}
}

// 中继是唯一没有协程兜底的循环（三条连接协程各自都有 recover），它一 panic 就是
// 进程级崩溃。兜住之后：进程活着，且等待回执的调用方必须拿到错误而不是一直等。
func TestBrokerPanicRepliesError(t *testing.T) {
	s := newTestHub(t, WithQueueStorage(&fakeStorage{dequeuePanic: true}))

	errCh := make(chan error, 1)
	sub := client.NewSubscription(client.NewConn(client.Config{}, nil, s), "/queue/jobs", "sub-1", frame.AckAuto)
	err := s.proc.submit(Request{
		Op:  SubscribeOp,
		Sub: sub,
		Err: errCh,
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	select {
	case got := <-errCh:
		if got == nil {
			t.Fatal("中继 panic 了，调用方却拿到 nil")
		}
	case <-time.After(time.Second):
		t.Fatal("中继 panic 后没有回执，调用方会一直等")
	}

	// 中继必须还在服务（进程没崩、循环没退出）。
	if err := s.proc.submit(Request{
		Op:    RequeueOp,
		Frame: frame.New(frame.MESSAGE, frame.Destination, "/queue/jobs"),
	}); err != nil {
		t.Fatalf("中继在 panic 后没有继续服务: %v", err)
	}
}

// 目的地由客户端任意构造，而 Manager 只增不删 —— 必须有个上限兜住内存增长。
func TestDestinationCap(t *testing.T) {
	const limit = 3

	s := newTestHub(t, WithMaxDestinations(limit))
	url := startTestServer(t, s)

	conn := dialTestClient(t, url)
	connect(t, conn, "")

	for i := 0; i < limit; i++ {
		id := fmt.Sprintf("sub-%d", i)
		writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
			"destination": fmt.Sprintf("/topic/r%d", i), "id": id, "receipt": id,
		})
		expectFrame(t, conn, frame.RECEIPT)
	}

	writeSTOMPFrame(t, conn, frame.SUBSCRIBE, map[string]string{
		"destination": "/topic/one-too-many", "id": "sub-x", "receipt": "rx",
	})
	expectFrame(t, conn, frame.ERROR)
}
