package eventbus

import (
	"sync"
	"testing"
	"time"
)

// waitSignal 在 d 内等一个信号；返回是否收到。
func waitSignal(t *testing.T, ch <-chan struct{}, d time.Duration) bool {
	t.Helper()
	select {
	case _, ok := <-ch:
		return ok
	case <-time.After(d):
		return false
	}
}

// TestNotifyReachesSubscriber 基本路径：Notify 后订阅者收到信号。
func TestNotifyReachesSubscriber(t *testing.T) {
	b := New()
	defer b.Close()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Notify()
	if !waitSignal(t, ch, time.Second) {
		t.Fatal("subscriber did not receive signal")
	}
}

// TestNotifyCoalescesBurst 合并窗口：一串连续 Notify 只产生一次广播。
//
// 这是核心设计目标——一次业务请求会触发多次状态变化
// （Acquire/Release/Record），前端不该被打扰 3~4 次去拉同一份状态。
func TestNotifyCoalescesBurst(t *testing.T) {
	b := New()
	defer b.Close()
	ch, cancel := b.Subscribe()
	defer cancel()

	// 窗口内连打 10 次：应只合并成 1 次广播。
	for i := 0; i < 10; i++ {
		b.Notify()
	}
	if !waitSignal(t, ch, time.Second) {
		t.Fatal("expected a signal from the burst")
	}
	// 不应再有第二个信号（窗口已合并）。
	if waitSignal(t, ch, 400*time.Millisecond) {
		t.Error("burst should coalesce into a single broadcast")
	}
}

// TestSeparateBurstsBroadcastAgain 分开的两批变化各广播一次。
//
// 与合并测试配对：证明"合并"没有退化成"永远只广播一次"。
func TestSeparateBurstsBroadcastAgain(t *testing.T) {
	b := New()
	defer b.Close()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Notify()
	if !waitSignal(t, ch, time.Second) {
		t.Fatal("first burst: no signal")
	}
	// 等过合并窗口，确保下一次是独立的一批。
	time.Sleep(3 * coalesceWindow)
	b.Notify()
	if !waitSignal(t, ch, time.Second) {
		t.Fatal("second burst: no signal")
	}
}

// TestNotifyNeverBlocks 热路径契约：Notify 必须永不阻塞。
//
// 它挂在请求关键路径上（pool.Acquire 用的是无锁原子计数器），
// 若这里会阻塞，等于给每个请求加了不确定延迟——绝对不可接受。
// 这里在"无人消费信号 + 反复 Notify"的极端下确认它立即返回。
func TestNotifyNeverBlocks(t *testing.T) {
	b := New()
	defer b.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 打远多于缓冲容量的次数，且不停下来。
		for i := 0; i < 10000; i++ {
			b.Notify()
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked — 热路径契约被破坏")
	}
}

// TestNotifyWithoutSubscribersIsCheap 无订阅者时 Notify 是空操作（不 panic、不阻塞）。
//
// 控制台没打开时每个请求仍会调 Notify，这条路径必须够便宜。
func TestNotifyWithoutSubscribersIsCheap(t *testing.T) {
	b := New()
	defer b.Close()
	for i := 0; i < 1000; i++ {
		b.Notify()
	}
	if n := b.Subscribers(); n != 0 {
		t.Errorf("subscribers=%d want 0", n)
	}
}

// TestNilBusSafe 未接线时（控制台关闭）全部方法安全。
//
// 让 handler 无需到处判空——这是刻意的 API 设计。
func TestNilBusSafe(t *testing.T) {
	var b *Bus
	b.Notify() // 不应 panic
	if b.Subscribers() != 0 {
		t.Error("nil bus should report 0 subscribers")
	}
	ch, cancel := b.Subscribe()
	cancel()
	if ch != nil {
		t.Error("nil bus should return nil channel (永不触发)")
	}
	b.Close() // 不应 panic
}

// TestCancelRemovesSubscriber cancel 后不再收到信号，且订阅计数归零。
func TestCancelRemovesSubscriber(t *testing.T) {
	b := New()
	defer b.Close()
	ch, cancel := b.Subscribe()
	if n := b.Subscribers(); n != 1 {
		t.Fatalf("subscribers=%d want 1", n)
	}
	cancel()
	if n := b.Subscribers(); n != 0 {
		t.Errorf("subscribers=%d want 0 after cancel", n)
	}
	// 取消后不应再收到信号（通道已从列表移除）。
	b.Notify()
	time.Sleep(3 * coalesceWindow)
	select {
	case <-ch:
		t.Error("cancelled subscriber must not receive signals")
	default:
	}
}

// TestCancelIsIdempotent cancel 可重复调用（HTTP handler 的 defer 可能多次触发）。
func TestCancelIsIdempotent(t *testing.T) {
	b := New()
	defer b.Close()
	_, cancel := b.Subscribe()
	cancel()
	cancel() // 不应 panic
	cancel()
}

// TestCloseReleasesSubscribers Close 后订阅者通道被关闭（前端读到 EOF 后重连）。
func TestCloseReleasesSubscribers(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()

	b.Close()
	// 通道应被 close → 读返回零值 + ok=false。
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel after Close")
		}
	case <-time.After(time.Second):
		t.Error("Close did not release subscriber channel")
	}
	// Close 幂等。
	b.Close()
}

// TestNotifyAfterCloseIsNoop Close 后 Notify 不 panic（进程退出竞态）。
func TestNotifyAfterCloseIsNoop(t *testing.T) {
	b := New()
	b.Close()
	for i := 0; i < 100; i++ {
		b.Notify()
	}
}

// TestMultipleSubscribersAllNotified 多个订阅者（多个标签页）都收到信号。
func TestMultipleSubscribersAllNotified(t *testing.T) {
	b := New()
	defer b.Close()
	const n = 3
	chans := make([]<-chan struct{}, n)
	for i := 0; i < n; i++ {
		ch, cancel := b.Subscribe()
		defer cancel()
		chans[i] = ch
	}
	if got := b.Subscribers(); got != n {
		t.Fatalf("subscribers=%d want %d", got, n)
	}
	b.Notify()
	for i, ch := range chans {
		if !waitSignal(t, ch, time.Second) {
			t.Errorf("subscriber %d did not receive signal", i)
		}
	}
}

// TestSlowSubscriberDoesNotBlockOthers 慢订阅者（未消费信号）不该拖住其他订阅者。
//
// 真实场景：一个标签页被切到后台/卡住，不能让它拖慢另一个活跃页面的实时性。
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	b := New()
	defer b.Close()

	// 慢订阅者：完全不去读通道（缓冲会被填满）。
	slow, cancelSlow := b.Subscribe()
	defer cancelSlow()
	fast, cancelFast := b.Subscribe()
	defer cancelFast()

	// 连打几批，每批之间等过窗口。
	for i := 0; i < 3; i++ {
		b.Notify()
		time.Sleep(3 * coalesceWindow)
	}
	// 快订阅者应能正常收到（不被慢订阅者阻塞）。
	if !waitSignal(t, fast, time.Second) {
		t.Error("fast subscriber starved by slow subscriber")
	}
	// 慢订阅者通道确实积压了未读信号（符合"丢弃而非阻塞"的预期）。
	if !waitSignal(t, slow, time.Second) {
		t.Error("slow subscriber should still hold at least one signal")
	}
}

// TestConcurrentSubscribeNotifyCancel 并发订阅/取消/通知不 panic、不死锁。
//
// 模拟真实场景：多个浏览器标签页反复刷新（订阅/取消），
// 同时请求热路径在不停 Notify。
func TestConcurrentSubscribeNotifyCancel(t *testing.T) {
	b := New()
	defer b.Close()

	stop := make(chan struct{})
	var storm sync.WaitGroup
	var subs sync.WaitGroup

	// 通知风暴：用**独立** WaitGroup——它只在 stop 关闭后退出。
	// 若与订阅者共用同一个 WaitGroup，先 Wait 再 close(stop) 会自锁（踩过）。
	storm.Add(1)
	go func() {
		defer storm.Done()
		for {
			select {
			case <-stop:
				return
			default:
				b.Notify()
			}
		}
	}()

	// 反复订阅/取消：模拟多个标签页反复刷新。
	for i := 0; i < 20; i++ {
		subs.Add(1)
		go func() {
			defer subs.Done()
			for j := 0; j < 20; j++ {
				ch, cancel := b.Subscribe()
				select { // 顺带消费一下，贴近真实订阅者行为
				case <-ch:
				default:
				}
				cancel()
			}
		}()
	}

	subs.Wait()
	close(stop)
	storm.Wait()
	// 等广播循环把最后一次广播跑完，避免 Close 与之竞态。
	time.Sleep(3 * coalesceWindow)
}
