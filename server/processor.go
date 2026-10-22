package server

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-stomp/stomp/v3/frame"

	"github.com/ve-weiyi/stompws/logws"
	"github.com/ve-weiyi/stompws/server/client"
	"github.com/ve-weiyi/stompws/server/queue"
	"github.com/ve-weiyi/stompws/server/topic"
)

// ErrBrokerClosed 表示中继已停止，请求没有被处理。
var ErrBrokerClosed = errors.New("broker closed")

// stopLockTimeout 是 stop 等待在途提交结束的上限。
const stopLockTimeout = 500 * time.Millisecond

// requestProcessor 是所有中继操作的唯一执行者：topic.Manager 与 queue.Manager 由它独占，
// 两个包里的裸 map 与 container/list 因此不需要锁。
//
// 订阅类请求带回执 —— 必须保证「RECEIPT 晚于订阅真正生效」，
// 否则 SubscribeOpt.Receipt 想消除的丢消息竞态会在这里复活。
type requestProcessor struct {
	ch  chan Request
	tm  *topic.Manager
	qm  *queue.Manager
	log logws.Logger

	done     chan struct{} // 关闭 = 中继开始收尾
	stopped  chan struct{} // 关闭 = 中继协程已退出
	stopOnce sync.Once
	// known 记录已创建的目的地，配合 maxDestinations 兜住无限增长。只在 processor 协程上访问。
	known map[string]struct{}
	// stopMu 把「提交」与「关停」扣在一起（提交持读锁覆盖「查 done + 入队」，
	// stop 取写锁后才关 done）：于是 submit 返回过 nil 的请求一定已在通道里，会被 drain 处理。
	stopMu sync.RWMutex
	// maxDestinations 限制中继允许创建的目的地数量（Manager 只增不删，目的地由客户端任意构造）。
	maxDestinations int
}

func newRequestProcessor(storage queue.Storage, log logws.Logger, maxDestinations int) *requestProcessor {
	return &requestProcessor{
		ch:      make(chan Request, 128),
		tm:      topic.NewManager(),
		qm:      queue.NewManager(storage),
		log:     log,
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		known:   make(map[string]struct{}),

		maxDestinations: maxDestinations,
	}
}

// allowDestination 判断是否允许创建这个目的地并登记。Manager 只增不删，而目的地由客户端
// 任意构造，到上限后拒绝新建（已存在的照常服务）。
func (p *requestProcessor) allowDestination(dest string) bool {
	if _, ok := p.known[dest]; ok {
		return true
	}
	if len(p.known) >= p.maxDestinations {
		p.log.Errorf("refusing to create destination %q: %d already exist", dest, len(p.known))
		return false
	}
	p.known[dest] = struct{}{}
	return true
}

// serve 跑中继循环。所有中继状态的读写都发生在这个 goroutine 上。
func (p *requestProcessor) serve() {
	defer close(p.stopped)

	for {
		select {
		case r := <-p.ch:
			p.handle(r)
		case <-p.done:
			// 已受理（submit 返回过 nil）的请求必须处理完再退出。
			p.drain()
			return
		}
	}
}

// drain 处理完通道里积压的请求。只由 serve 在收到 done 之后调用。
func (p *requestProcessor) drain() {
	for {
		select {
		case r := <-p.ch:
			p.handle(r)
		default:
			return
		}
	}
}

// stop 让中继循环退出，可重复调用（请求通道刻意不关闭：关它会让并发提交方 panic，
// 而 done 通知下提交方拿到的是可处理的 ErrBrokerClosed）。
//
// 取写锁带超时：中继若卡在某个请求里（慢 storage/慢 logger），在途提交者会一直握着读锁，
// 不能跟着永久等 —— 否则 Shutdown 的超时承诺失效。代价：这种极端情况下
// 「submit 返回 nil 必被处理」退化为尽力而为。
func (p *requestProcessor) stop() {
	p.stopOnce.Do(func() {
		locked := false
		deadline := time.Now().Add(stopLockTimeout)

		for {
			if p.stopMu.TryLock() {
				locked = true
				break
			}
			if time.Now().After(deadline) {
				p.log.Warningf("broker did not quiesce within %v; stopping anyway", stopLockTimeout)
				break
			}
			time.Sleep(time.Millisecond)
		}

		close(p.done)

		if locked {
			p.stopMu.Unlock()
		}
	})
}

// submit 提交一个请求，只保证入队、不等处理结果。返回 nil 的语义是「中继会处理它」
// （关停时积压的由 drain 兜底），关停后再提交一律拿到 ErrBrokerClosed。
func (p *requestProcessor) submit(r Request) error {
	p.stopMu.RLock()
	defer p.stopMu.RUnlock()

	select {
	case <-p.done:
		return ErrBrokerClosed
	default:
	}

	select {
	case p.ch <- r:
		return nil
	case <-p.done:
		return ErrBrokerClosed
	}
}

// call 提交一个需要结果的请求并等它完成（订阅类操作走这里，调用方要确认订阅已挂上）。
// 同时监听 done：中继可能取走请求后未处理就退出，只等 errCh 会永久阻塞。
func (p *requestProcessor) call(op RequestOp, sub *client.Subscription) error {
	errCh := make(chan error, 1)
	if err := p.submit(Request{Op: op, Sub: sub, Err: errCh}); err != nil {
		return err
	}

	select {
	case err := <-errCh:
		return err
	case <-p.done:
		return ErrBrokerClosed
	}
}

func (p *requestProcessor) handle(r Request) {
	// 中继是唯一没有协程兜底的循环，panic 即进程级崩溃。兜住的同时必须把结果回给
	// 等待中的调用方，否则它会一直等一个不会来的回执。
	defer func() {
		if rec := recover(); rec != nil {
			p.log.Errorf("broker panic on %s: %v", r.Op, rec)
			r.reply(fmt.Errorf("broker panic on %s: %v", r.Op, rec))
		}
	}()

	switch r.Op {
	case SubscribeOp:
		r.reply(p.subscribe(r.Sub))

	case UnsubscribeOp:
		r.reply(p.unsubscribe(r.Sub))

	case EnqueueOp:
		dest := r.Frame.Header.Get(frame.Destination)
		if err := p.enqueue(r.Frame); err != nil {
			p.log.Errorf("enqueue to %s failed: %v", dest, err)
		}

	case RequeueOp:
		dest := r.Frame.Header.Get(frame.Destination)
		if err := p.requeue(r.Frame); err != nil {
			p.log.Errorf("requeue to %s failed: %v", dest, err)
		}
	}
}

// subscribe 把订阅挂到对应目的地。挂之前必须确认订阅仍有效：退订发生在 processLoop、
// 挂回队列池发生在这里，先后无法在连接侧保证，一个在途的重订阅就能把死订阅挂回去 ——
// 而队列「一条消息只给一个消费者」，它会替活订阅吃掉消息。
func (p *requestProcessor) subscribe(sub *client.Subscription) error {
	if !sub.IsActive() {
		p.log.Infof("skip resubscribe of inactive subscription %s: %s", sub.Id(), sub.Destination())
		return nil
	}

	dest := sub.Destination()
	if !p.allowDestination(dest) {
		return errTooManyDestinations
	}

	switch {
	case isTopicDestination(dest):
		p.tm.Find(dest).Subscribe(sub)
	case isQueueDestination(dest):
		return p.qm.Find(dest).Subscribe(sub)
	default:
		return errInvalidDestination
	}
	return nil
}

// unsubscribe 把订阅从对应目的地摘掉。
func (p *requestProcessor) unsubscribe(sub *client.Subscription) error {
	dest := sub.Destination()
	switch {
	case isTopicDestination(dest):
		p.tm.Find(dest).Unsubscribe(sub)
	case isQueueDestination(dest):
		p.qm.Find(dest).Unsubscribe(sub)
	default:
		return errInvalidDestination
	}
	return nil
}

// enqueue 投递一条消息。
func (p *requestProcessor) enqueue(f *frame.Frame) error {
	dest := f.Header.Get(frame.Destination)
	if !p.allowDestination(dest) {
		return errTooManyDestinations
	}

	switch {
	case isQueueDestination(dest):
		return p.qm.Find(dest).Enqueue(f)
	case isTopicDestination(dest):
		p.tm.Find(dest).Enqueue(f)
		return nil
	default:
		return errInvalidDestination
	}
}

// requeue 把未能送达的消息放回队首。topic 是 fire-and-forget，没有待确认状态。
func (p *requestProcessor) requeue(f *frame.Frame) error {
	dest := f.Header.Get(frame.Destination)
	if !isQueueDestination(dest) {
		return nil
	}
	return p.qm.Find(dest).Requeue(f)
}

// requeueDirect 是中继协程自己用的退回入口（投递失败发生在 processor 内部时，
// 往 p.ch 提交会自锁）。调用方必须已经在 processor 协程上。
func (p *requestProcessor) requeueDirect(dest string, f *frame.Frame) {
	if !isQueueDestination(dest) {
		return
	}
	if err := p.qm.Find(dest).Requeue(f); err != nil {
		p.log.Errorf("requeue to %s failed: %v", dest, err)
	}
}
