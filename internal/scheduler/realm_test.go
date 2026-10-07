package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

// notFoundServer 所有请求回 404（模拟"该产品线没有这个运营接口"）。
func notFoundServer(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRealmPool(rn string) *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, Realm: rn})
	return p
}

// TestUnsupportedTaskSkippedOn404 非 cn 线遇到 404（路径不存在）→ 标记 unsupported，
// 后续同一任务不再打上游（避免对不存在的接口反复发无望请求）。
func TestUnsupportedTaskSkippedOn404(t *testing.T) {
	fastActivity(t)
	var calls atomic.Int32
	srv := notFoundServer(t, &calls)

	s := New(Config{
		Pool: newRealmPool(realm.WB),
		Upstream: &upstream.Client{
			HTTP: srv.Client(),
			// workbuddy 线的 base 走 realm 档案覆盖（ChatBaseCN 只影响 cn 线，这是刻意的：
			// 历史字段保持 cn 语义，新线用 Profiles 注入，避免互相串味）。
			Profiles: map[string]realm.Profile{realm.WB: {ChatBase: srv.URL, BillingBase: srv.URL}},
		},
		Realm: realm.WB,
	})
	s.RunActivityNow()
	first := calls.Load()
	if first == 0 {
		t.Fatal("首轮应打过上游")
	}
	if _, skip := s.unsupportedReason(taskActivity); !skip {
		t.Fatal("404 后 activity 应被标记 unsupported")
	}
	s.RunActivityNow()
	if calls.Load() != first {
		t.Errorf("第二轮不该再打上游: calls=%d want %d", calls.Load(), first)
	}
}

// TestCNRealmNoAutoSkip cn 线（含未标注 Realm 的既有部署）不启用自动跳过：
// 偶发 404 交给既有短冷却，行为与改造前一致。
func TestCNRealmNoAutoSkip(t *testing.T) {
	fastActivity(t)
	var calls atomic.Int32
	srv := notFoundServer(t, &calls)

	s := New(Config{
		Pool:     newRealmPool(realm.CN),
		Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
		// 不设 Realm → 视为 cn
	})
	s.RunActivityNow()
	first := calls.Load()
	s.RunActivityNow()
	if calls.Load() <= first {
		t.Errorf("cn 线不该被永久跳过: calls=%d → %d", first, calls.Load())
	}
	if _, skip := s.unsupportedReason(taskActivity); skip {
		t.Error("cn 线不该标记 unsupported")
	}
}

// TestNoteUnsupportedIgnoresServerError 5xx 是临时故障，不该被当成"接口不存在"永久跳过。
func TestNoteUnsupportedIgnoresServerError(t *testing.T) {
	s := New(Config{Realm: realm.WB})
	s.noteUnsupported(taskActivity, &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"})
	if _, skip := s.unsupportedReason(taskActivity); skip {
		t.Error("5xx 不该标记 unsupported")
	}
	s.noteUnsupported(taskActivity, &upstream.Error{Kind: upstream.ErrNotFound, Status: 404, Msg: "404 page not found"})
	if _, skip := s.unsupportedReason(taskActivity); !skip {
		t.Error("404 应标记 unsupported")
	}
}

// TestTaskKindString 任务名与配置键一一对应（日志/配置同名，便于对照）。
func TestTaskKindString(t *testing.T) {
	cases := map[taskKind]string{
		taskCheckin:   "checkin",
		taskTravel:    "travel",
		taskActivity:  "activity",
		taskKeepalive: "keepalive",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("taskKind(%d).String()=%q want %q", k, got, want)
		}
	}
}
