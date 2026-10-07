package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// balanceStub 模拟余额查询接口：按 uid 返回不同余额，并统计调用次数。
func balanceStub(t *testing.T, byUID map[string]int64, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		uid := r.Header.Get("X-User-Id")
		remain, ok := byUID[uid]
		if !ok {
			remain = 0
		}
		body := fmt.Sprintf(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"PackageName":"p","CycleCapacitySize":500,"CycleCapacityRemain":%d,"CycleCapacityUsed":0}]}}}}`, remain)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func creditWatchPool(uids ...string) *pool.Pool {
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	return p
}

// TestCreditWatchFreezesZeroBalance 全量巡检：余额为 0 的账号被主动冻结
// （不必等请求撞 ErrHardCredit），余额充足的保持可用。
func TestCreditWatchFreezesZeroBalance(t *testing.T) {
	creditWatchAccountDelay = 0
	var calls atomic.Int32
	srv := balanceStub(t, map[string]int64{"u-rich": 120, "u-poor": 0}, &calls)

	p := creditWatchPool("u-rich", "u-poor")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CreditWatchEnabled: true, CreditWatchScope: "all"})

	froze, unfroze := s.RunCreditWatchNow(true)
	if froze != 1 || unfroze != 0 {
		t.Fatalf("froze=%d unfroze=%d want 1/0", froze, unfroze)
	}
	if !p.IsFrozen("u-poor") {
		t.Error("余额 0 的账号应被冻结")
	}
	if p.IsFrozen("u-rich") {
		t.Error("余额充足的账号不该被冻结")
	}
	if st, _ := p.Status("u-rich"); st.Credits != 120 {
		t.Errorf("额度缓存未刷新: %d", st.Credits)
	}
}

// TestCreditWatchUnfreezesOnRecovery 冻结账号在余额恢复（如签到/套餐刷新）后自动解冻。
func TestCreditWatchUnfreezesOnRecovery(t *testing.T) {
	creditWatchAccountDelay = 0
	var calls atomic.Int32
	// 第一轮：余额 0 → 冻结
	srv := balanceStub(t, map[string]int64{"u1": 0}, &calls)
	p := creditWatchPool("u1")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CreditWatchEnabled: true, CreditWatchScope: "frozen"})

	if froze, _ := s.RunCreditWatchNow(true); froze != 1 {
		t.Fatalf("首轮应冻结 1 个，got %d", froze)
	}

	// 第二轮：余额恢复 30 → 解冻（范围 frozen 也会查它，因为它是冻结账号）
	srv2 := balanceStub(t, map[string]int64{"u1": 30}, &calls)
	s.cfg.Upstream = &upstream.Client{HTTP: srv2.Client(), ChatBaseCN: srv2.URL, BillingBaseCN: srv2.URL}
	froze, unfroze := s.RunCreditWatchNow(false)
	if froze != 0 || unfroze != 1 {
		t.Fatalf("恢复后应解冻: froze=%d unfroze=%d", froze, unfroze)
	}
	if p.IsFrozen("u1") {
		t.Error("解冻后不应仍为冻结")
	}
	if a := p.Pick(); a == nil {
		t.Error("解冻后应可被选中")
	}
}

// TestCreditWatchScopeFrozenSkipsHealthy 范围=frozen 时只查冻结账号，
// 不对健康账号发无谓的余额查询。
func TestCreditWatchScopeFrozenSkipsHealthy(t *testing.T) {
	creditWatchAccountDelay = 0
	var calls atomic.Int32
	srv := balanceStub(t, map[string]int64{"u1": 50, "u2": 50}, &calls)
	p := creditWatchPool("u1", "u2")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CreditWatchEnabled: true, CreditWatchScope: "frozen"})

	// forceAll=false 且无冻结账号 → 一个请求都不该发
	s.RunCreditWatchNow(false)
	if n := calls.Load(); n != 0 {
		t.Errorf("范围 frozen 且无冻结账号时不该查询上游，got %d 次", n)
	}

	// 冻结一个后，只查它
	p.FreezeNoCredits("u2", "余额不足")
	calls.Store(0)
	s.RunCreditWatchNow(false)
	if n := calls.Load(); n != 1 {
		t.Errorf("应只查 1 个冻结账号，got %d 次", n)
	}
}

// TestCreditWatchQueryFailureKeepsState 查询失败不改状态：一次网络抖动不该把冻结号解冻
// （否则会放进一个余额为 0 的号去撞 429）。
func TestCreditWatchQueryFailureKeepsState(t *testing.T) {
	creditWatchAccountDelay = 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()

	p := creditWatchPool("u1")
	p.FreezeNoCredits("u1", "余额不足")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, CreditWatchEnabled: true, CreditWatchScope: "frozen"})

	s.RunCreditWatchNow(false)
	if !p.IsFrozen("u1") {
		t.Error("查询失败时不该解冻")
	}
}

var _ = json.Marshal
