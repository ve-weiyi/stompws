package client

import (
	"sync"
	"time"

	"github.com/go-stomp/stomp/v3/frame"
)

// fakeHub 是 Hub 接口的测试替身：记录收到的调用，供连接层的白盒测试断言
// 「连接层有没有按契约把事交给中继」。
//
// 真实实现（*server.StompHubServer）与它的行为测试在 server 包 ——
// 连接层的测试只需要替身，因为断言对象是这条边界，而不是中继内部。

type fakeHub struct {
	mu sync.Mutex

	attached       []*Subscription
	detached       []*Subscription
	published      []*frame.Frame
	requeued       []*frame.Frame
	requeuedDirect []*frame.Frame
	acked          []string
	nacked         []string

	connected    int
	disconnected int
	goroutines   int

	// 可注入的行为
	attachErr     error
	detachErr     error
	rejectPublish bool

	done chan struct{}
}

func newFakeHub() *fakeHub {
	return &fakeHub{done: make(chan struct{})}
}

func (b *fakeHub) Attach(sub *Subscription) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attachErr != nil {
		return b.attachErr
	}
	b.attached = append(b.attached, sub)
	return nil
}

func (b *fakeHub) Detach(sub *Subscription) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.detachErr != nil {
		return b.detachErr
	}
	b.detached = append(b.detached, sub)
	return nil
}

func (b *fakeHub) Publish(_ *Conn, f *frame.Frame) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rejectPublish {
		return false
	}
	b.published = append(b.published, f)
	return true
}

func (b *fakeHub) Requeue(f *frame.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requeued = append(b.requeued, f)
}

func (b *fakeHub) RequeueDirect(f *frame.Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requeuedDirect = append(b.requeuedDirect, f)
}

func (b *fakeHub) Acked(_ *Conn, messageID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.acked = append(b.acked, messageID)
}

func (b *fakeHub) Nacked(_ *Conn, messageID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nacked = append(b.nacked, messageID)
}

func (b *fakeHub) Connected(*Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected++
}

func (b *fakeHub) Disconnected(*Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disconnected++
}

func (b *fakeHub) GoroutineDone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.goroutines++
}

func (b *fakeHub) Done() <-chan struct{} {
	return b.done
}

// ──────────────── 断言帮手 ────────────────

func (b *fakeHub) tookRequeue() []*frame.Frame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*frame.Frame(nil), b.requeued...)
}

// waitRequeue 等到至少 n 次退回（退回多发生在别的协程上）。
func (b *fakeHub) waitRequeue(t interface{ Fatalf(string, ...any) }, n int) []*frame.Frame {
	for i := 0; i < 200; i++ {
		if got := b.tookRequeue(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等不到 %d 次 Requeue", n)
	return nil
}
