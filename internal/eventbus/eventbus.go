// eventbus 控制台的"状态有变"广播总线。
//
// 用途：把网关内部的状态变化（在途数、使用日志、账号健康/冷却/冻结）
// 以 SSE 推给控制台，替代定频轮询，让页面在变化发生时立即更新。
//
// # 为什么是"合并广播"而不是"逐事件投递"
//
// 一次业务请求会触发多次变化（Acquire → 上游返回 → Release → Record），
// 每次单独推一条会让前端在同一瞬间收到 3~4 条通知，然后重复拉取同一份状态。
// 所以本总线把短时间内的多次 Notify **合并成一次**广播：
// 订阅者只被告知"有变化，来取最新状态"，不关心具体变了几次、变了什么。
//
// 这个语义刻意做成"状态拉取信号"而非"增量事件流"：
//   - 不需要为每种事件定义 schema，新增状态字段时前端不用改；
//   - 天然幂等——漏掉一次通知只意味着晚一点看到，不会状态错乱；
//   - 前端的兜底轮询与它无缝配合（两者走同一个"取全量"路径）。
//
// # 并发契约（关键）
//
// Notify 必须可以被**任意 goroutine 无锁调用**，且**永不阻塞**——
// 它会被挂在请求热路径上（pool.Acquire/Release 用的是无锁原子计数器，
// 不能为了通知再去争锁）。为此：
//   - Notify 只做一次带缓冲的非阻塞投递，缓冲满就直接丢弃（已有待处理信号）；
//   - 广播由独立 goroutine 完成，订阅者的慢消费不会反压到业务路径；
//   - 向订阅者投递也是非阻塞的，慢订阅者丢信号而非卡住广播。
//
// 丢弃是安全的：信号是"去取最新状态"，丢一个信号只代表少一次拉取机会，
// 后面的变化会再次触发。前端另有兜底轮询，见 console.html 的 scheduleAccounts。
package eventbus

import (
	"sync"
	"sync/atomic"
	"time"
)

// coalesceWindow 合并窗口：窗口内到达的多次 Notify 只广播一次。
//
// 取值权衡：太小则一次业务请求的通知被拆成多条（浪费往返）；
// 太大则用户感知的延迟增加。120ms 略大于一次本地 HTTP 往返，
// 足以把 Acquire/Release/Record 合并成一条，同时仍在人眼"即时"范围内。
const coalesceWindow = 120 * time.Millisecond

// subChanCap 单个订阅者的信号缓冲。
// 1 就够：订阅者要的是"有没有变化"，攒多个信号没有意义
// （取一次就是最新状态）。满了即丢弃，见包注释。
const subChanCap = 1

// Bus 状态变化广播总线。零值不可用，用 New 构造。
type Bus struct {
	mu   sync.Mutex
	subs map[int64]chan struct{}
	next int64

	// wake 合并窗口的触发信号（缓冲 1，非阻塞投递）。
	wake chan struct{}
	// closed 关闭标记：Close 后 Notify 变空操作、广播循环退出。
	closed atomic.Bool
}

// New 构造总线并启动广播循环。
//
// 广播循环是常驻 goroutine，Close 时退出——生产只在进程退出时关闭，
// 测试里必须关（否则 goroutine 泄漏会被 -race 或 goleak 抓到）。
func New() *Bus {
	b := &Bus{
		subs: map[int64]chan struct{}{},
		wake: make(chan struct{}, 1),
	}
	go b.loop()
	return b
}

// Notify 报告"状态有变"。可在任意 goroutine、持锁或无锁环境下调用，永不阻塞。
//
// 这是业务代码唯一需要关心的 API：调用点只管喊一声，不关心有没有订阅者、
// 有没有人在看控制台——没有订阅者时开销就是一次带缓冲 channel 的非阻塞发送。
func (b *Bus) Notify() {
	if b == nil || b.closed.Load() {
		return
	}
	select {
	case b.wake <- struct{}{}:
	default:
		// 已有待处理的唤醒信号：本次变化会被那次广播覆盖（合并窗口内），
		// 无需再投递。
	}
}

// Subscribe 订阅变化信号，返回只读信号通道与取消函数。
//
// 收到信号表示"该去取最新状态了"，不携带任何内容。
// 必须调用 cancel 释放（否则该订阅者会一直被广播循环引用，造成泄漏）。
func (b *Bus) Subscribe() (<-chan struct{}, func()) {
	if b == nil {
		// 未接线（如控制台关闭）：返回一个永不触发的通道，
		// 让调用方无需判空（handler 因此可以无条件 select）。
		return nil, func() {}
	}
	ch := make(chan struct{}, subChanCap)
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			 // 不 close(ch)：可能仍有并发广播正在向它投递，
			// close 会 panic。取消后该通道不再被引用，由 GC 回收即可。
		})
	}
}

// Subscribers 当前订阅者数量（测试与观测用）。
func (b *Bus) Subscribers() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Close 停止广播循环并释放全部订阅者（幂等）。
func (b *Bus) Close() {
	if b == nil || !b.closed.CompareAndSwap(false, true) {
		return
	}
	b.mu.Lock()
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
	b.mu.Unlock()
	// 唤醒循环让它看到 closed 并退出。
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// loop 广播循环：等唤醒 → 静默 coalesceWindow 收集后续变化 → 广播一次。
func (b *Bus) loop() {
	for range b.wake {
		if b.closed.Load() {
			return
		}
		// 合并窗口：期间到达的 Notify 会被下面的 drain 吸收。
		time.Sleep(coalesceWindow)
		// 清掉窗口期间积累的唤醒信号（只广播一次就够）。
		select {
		case <-b.wake:
		default:
		}
		b.broadcast()
		if b.closed.Load() {
			return
		}
	}
}

// broadcast 向全部订阅者投递一次信号（非阻塞，慢订阅者丢弃）。
func (b *Bus) broadcast() {
	b.mu.Lock()
	targets := make([]chan struct{}, 0, len(b.subs))
	for _, ch := range b.subs {
		targets = append(targets, ch)
	}
	b.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- struct{}{}:
		default:
			// 该订阅者已有未读信号（前端还没取完上一轮）：丢弃。
			// 它取完会看到新状态，因为信号语义是"取最新"，不是"逐条消费"。
		}
	}
}
