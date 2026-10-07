package pool

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// fakeNotifier 记录 Notify 调用次数（并发安全）。
type fakeNotifier struct {
	n atomic.Int64
	// block 非空时 Notify 会阻塞等待它（用于验证"持锁中通知不会死锁"这一反向场景——
	// 真实实现永不阻塞，但测试要确认即便实现阻塞，也不会因为在持锁时调用
	// 而把自己锁死；这是对 markDirty 契约的强化检查）。
	block chan struct{}
}

func (f *fakeNotifier) Notify() {
	if f.block != nil {
		<-f.block
	}
	f.n.Add(1)
}

func (f *fakeNotifier) count() int64 { return f.n.Load() }

// TestMarkDirtyNotifiesOnStateChange 状态变更会通知（冻结/冷却/额度写入）。
//
// 这是实时推送的数据源：所有落盘级状态变更都应触发一次通知，
// 否则控制台会等到兜底轮询才发现变化。
func TestMarkDirtyNotifiesOnStateChange(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	before := f.count()
	// 额度写入（SetCredits → markDirty）
	p.SetCredits("u1", 100)
	if f.count() <= before {
		t.Error("SetCredits did not notify")
	}

	// 冷却（Cooldown → markDirty）
	before = f.count()
	p.Cooldown("u1", CoolSoft, time.Minute, "test")
	if f.count() <= before {
		t.Error("Cooldown did not notify")
	}

	// 冻结（ReconcileCredits 到 0 → markDirty）
	before = f.count()
	p.ReconcileCredits("u1", 0)
	if f.count() <= before {
		t.Error("ReconcileCredits did not notify")
	}

	// 人工停用（SetManualDisabled → markDirty）
	before = f.count()
	p.SetManualDisabled("u1", true)
	if f.count() <= before {
		t.Error("SetManualDisabled did not notify")
	}
}

// TestAcquireReleaseNotifies 在途数变化会通知。
//
// 在途计数走无锁原子路径（不碰 dirty），所以它有独立的通知点；
// 这条测试与 markDirty 那条互补，共同保证"在途"实时。
func TestAcquireReleaseNotifies(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	before := f.count()
	if !p.Acquire("u1") {
		t.Fatal("Acquire failed")
	}
	if f.count() <= before {
		t.Error("Acquire did not notify")
	}

	before = f.count()
	p.Release("u1")
	if f.count() <= before {
		t.Error("Release did not notify")
	}
}

// TestAcquireRejectedDoesNotNotify 占名额被拒时不该通知。
//
// 状态没变就通知会让满额账号在高并发下反复唤醒控制台（无谓的拉取）。
func TestAcquireRejectedDoesNotNotify(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	if !p.Acquire("u1") { // 占满唯一名额
		t.Fatal("first Acquire should succeed")
	}
	before := f.count()
	if p.Acquire("u1") { // 第二次应被拒
		t.Fatal("second Acquire should be rejected")
	}
	if f.count() != before {
		t.Error("rejected Acquire must not notify (state unchanged)")
	}
}

// TestReleaseAtZeroDoesNotNotify Release 的幂等 no-op 分支不该通知。
func TestReleaseAtZeroDoesNotNotify(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	// 在途为 0 时 Release 是 no-op（不该通知）。
	before := f.count()
	p.Release("u1")
	if f.count() != before {
		t.Error("Release at zero must not notify (nothing changed)")
	}
}

// TestMarkDirtyUnderLockDoesNotDeadlock 在**持写锁时**通知不能死锁。
//
// 这是本项目最容易踩的坑：绝大多数状态变更（Cooldown/Reconcile/Disable…）
// 都在 p.mu.Lock() 之内，而 Go 的 RWMutex 不可重入——若 notifyChange 用
// RLock 读字段，同一个 goroutine 持写锁再取读锁会**永久死锁整个网关**。
// 故实现用 atomic.Pointer。本测试若挂起，说明这个不变量被破坏了。
func TestMarkDirtyUnderLockDoesNotDeadlock(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 这些方法都在内部持写锁后调用 markDirty → notifyChange。
		p.SetCredits("u1", 50)
		p.Cooldown("u1", CoolSoft, time.Minute, "x")
		p.ReconcileCredits("u1", 0)
		p.Disable("u1", "x")
		p.NoteError("u1")
		p.NoteSuccess("u1")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: state change while holding write lock blocked forever " +
			"(notifyChange must not take the pool lock)")
	}
	if f.count() == 0 {
		t.Error("expected notifications from lock-held state changes")
	}
}

// TestNotifyUnderLockWithBlockingNotifier 强化检查：即便通知器会阻塞，
// 持锁调用也不该让"池自身的锁"被永久占用（通知在锁内，但池锁最终会释放）。
//
// 真实实现（eventbus）永不阻塞，这里用一个会阻塞的 fake 确认：
// 阻塞只影响当前 goroutine 的进度，不会污染池的锁状态——持锁期间调用
// notifyChange 不会与任何其他 goroutine 形成环。
func TestNotifyUnderLockWithBlockingNotifier(t *testing.T) {
	p := New("")
	release := make(chan struct{})
	f := &fakeNotifier{block: release}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})

	started := make(chan struct{})
	go func() {
		close(started)
		p.SetCredits("u1", 10) // 持锁 → notifyChange → fake 阻塞
	}()

	<-started
	// 给 goroutine 一点时间进入阻塞的 Notify。
	time.Sleep(100 * time.Millisecond)

	// 关键断言：池的**读操作**仍可完成吗？不可以——因为 SetCredits 持写锁，
	// 而它在锁内阻塞了。这是"通知不应在锁内做重活"的原因，
	// 但注意 eventbus.Notify 是非阻塞投递，生产路径不会这样阻塞。
	// 本测试只确认：放行后一切能恢复正常（没有永久死锁）。
	close(release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		done := make(chan struct{})
		go func() { p.Status("u1"); close(done) }()
		select {
		case <-done:
			return // 恢复正常
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("pool lock never released after notifier unblocked")
}

// TestSetChangeNotifierNilDisables 注入 nil 关闭通知（控制台关闭时）。
func TestSetChangeNotifierNilDisables(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1)
	if f.count() == 0 {
		t.Fatal("precondition: notifier should have been called")
	}

	p.SetChangeNotifier(nil)
	before := f.count()
	p.SetCredits("u1", 2) // 不应 panic，也不应再通知
	if f.count() != before {
		t.Error("nil notifier should disable notifications")
	}
}

// TestNoNotifierIsSafe 未注入通知器时一切照常（测试与未接控制台的场景）。
func TestNoNotifierIsSafe(t *testing.T) {
	p := New("") // 不注入
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 10)
	p.Acquire("u1")
	p.Release("u1")
	p.Cooldown("u1", CoolSoft, time.Minute, "x")
	// 到这里不 panic 即通过。
}

// TestConcurrentNotifyDoesNotRace 并发状态变更 + 注入/清除通知器不 panic。
//
// 验证 atomic.Pointer 的读写安全（-race 不可用于本平台，靠多变体覆盖）。
func TestConcurrentNotifyDoesNotRace(t *testing.T) {
	p := New("")
	f := &fakeNotifier{}
	p.SetChangeNotifier(f)
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: "u" + string(rune('a'+i))})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 变更风暴。
	for i := 0; i < 5; i++ {
		uid := "u" + string(rune('a'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					p.SetCredits(uid, 100)
					p.Acquire(uid)
					p.Release(uid)
				}
			}
		}()
	}
	// 同时反复换通知器（含 nil）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			p.SetChangeNotifier(f)
			p.SetChangeNotifier(nil)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}
