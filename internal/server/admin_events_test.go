package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/eventbus"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
)

// sseProbe 可编程的事件源：测试手动触发信号，不依赖真实总线。
type sseProbe struct {
	mu   sync.Mutex
	ch   chan struct{}
	subs int
	// closed 记录订阅通道是否已被关闭（用于断言 handler 退出行为）。
	closed bool
}

func newSSEProbe() *sseProbe {
	return &sseProbe{ch: make(chan struct{}, 1)}
}

func (p *sseProbe) Subscribe() (<-chan struct{}, func()) {
	p.mu.Lock()
	p.subs++
	p.mu.Unlock()
	return p.ch, func() {
		p.mu.Lock()
		p.subs--
		p.mu.Unlock()
	}
}

// fire 触发一次信号。
func (p *sseProbe) fire() {
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

// TestSSEUnauthorized SSE 端点必须走与其他管理接口相同的鉴权。
//
// 若这里能绕过，等于把整个控制台的实时数据流对外裸奔。
func TestSSEUnauthorized(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	req := httptest.NewRequest("GET", "/admin/api/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401 (SSE must require the same auth)", rec.Code)
	}
}

// TestSSEHeadersAndConnectFrame 连接建立时立即写出 SSE 头与连接确认帧。
//
// 为什么必须有"立即写出"：前端要能区分"连上了但暂时无变化"与"根本没连上"。
// 若首帧要等到第一次状态变化才发，用户会以为实时功能坏了。
func TestSSEHeadersAndConnectFrame(t *testing.T) {
	probe := newSSEProbe()
	h := sseTestHandler(t, probe)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("cache-control=%q want no-store", resp.Header.Get("Cache-Control"))
	}
	// 关闭代理缓冲：否则 Nginx/Cloudflare 下事件会被攒住不发。
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("x-accel-buffering=%q want no (proxy buffering must be disabled)", got)
	}

	// 首帧应是注释形式的连接确认。
	line := readSSELine(t, resp.Body, 2*time.Second)
	if !strings.HasPrefix(line, ":") {
		t.Errorf("first frame=%q want a comment line (: connected)", line)
	}
}

// TestSSEDeliversChangeOnNotify 状态变化时推送 change 事件。
func TestSSEDeliversChangeOnNotify(t *testing.T) {
	probe := newSSEProbe()
	h := sseTestHandler(t, probe)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()
	readSSELine(t, resp.Body, 2*time.Second) // 吃掉连接确认帧

	probe.fire()

	// 读到 event: change。
	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		l := readSSELine(t, resp.Body, time.Second)
		if strings.HasPrefix(l, "event:") {
			got = l
			break
		}
	}
	if got != "event: change" {
		t.Errorf("event line=%q want \"event: change\"", got)
	}
}

// TestSSEPayloadCarriesState 事件必须带上状态负载——这是"推取代拉"的核心契约。
//
// 断言三件事：
//  1. data 行是合法 JSON 且含 overview；
//  2. 负载内容与 /admin/api/overview 一致（"推"与"拉"同口径，防止两条路各写一份而跑偏）；
//  3. data 行内没有裸换行（否则 SSE 帧会被撕碎，前端只会收到半截 JSON）。
func TestSSEPayloadCarriesState(t *testing.T) {
	probe := newSSEProbe()
	// 直接构造 *Handler：sseTestHandler 返回的是 http.Handler，而下面还要再走一次
	// adminGet 断言"推与拉同口径"，那个入口要的是 *Handler。
	p := testPoolWith(&auth.Auth{UID: "u-sse", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "sse"})
	h := NewHandler(Config{Pool: p, APIKey: "sk-master", Events: probe, ConsoleEnabled: true})

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()
	readSSELine(t, resp.Body, 2*time.Second) // 连接确认帧

	probe.fire()

	dataLine := ""
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && dataLine == "" {
		if l := readSSELine(t, resp.Body, time.Second); strings.HasPrefix(l, "data: ") {
			dataLine = l
		}
	}
	if dataLine == "" {
		t.Fatal("change 事件里没有 data 行")
	}
	raw := strings.TrimPrefix(dataLine, "data: ")
	if strings.ContainsAny(raw, "\r\n") {
		t.Fatalf("data 行内出现裸换行，SSE 帧会被撕碎: %q", raw)
	}
	var payload struct {
		Overview json.RawMessage `json:"overview"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v (raw=%s)", err, raw)
	}
	if len(payload.Overview) == 0 {
		t.Fatal("payload 缺 overview")
	}

	var got any
	if err := json.Unmarshal(payload.Overview, &got); err != nil {
		t.Fatalf("overview 解析失败: %v", err)
	}
	code, want := adminGet(t, h, "/admin/api/overview", "sk-master")
	if code != 200 {
		t.Fatalf("overview code=%d", code)
	}
	// now 是"生成时刻"，两次调用必然不同——它是版本号不是内容，比较前剔除。
	if m, ok := got.(map[string]any); ok {
		delete(m, "now")
	}
	delete(want, "now")
	if !reflect.DeepEqual(got, want) {
		t.Error("SSE 负载与 /admin/api/overview 不一致——两条路各写了一份口径")
	}
}

// TestSSEMultipleNotifiesDeliverMultipleEvents 多次变化各推一次（不丢、不合并到前端看不见）。
//
// 注意：总线层会合并短窗口内的密集变化，但本测试直接操作 probe，
// 验证的是端点本身"来一个信号发一个事件"的转发行为。
func TestSSEMultipleNotifiesDeliverMultipleEvents(t *testing.T) {
	probe := newSSEProbe()
	h := sseTestHandler(t, probe)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()
	readSSELine(t, resp.Body, 2*time.Second) // 连接确认帧

	for i := 0; i < 3; i++ {
		probe.fire()
		// 每个信号应产生一个 event 行。
		deadline := time.Now().Add(2 * time.Second)
		found := false
		for time.Now().Before(deadline) && !found {
			l := readSSELine(t, resp.Body, time.Second)
			if strings.HasPrefix(l, "event:") {
				found = true
			}
		}
		if !found {
			t.Fatalf("notify %d: no event received", i)
		}
	}
}

// TestSSEUnsubscribesOnClientDisconnect 客户端断开后必须取消订阅。
//
// 不取消 = 每次刷新页面都在总线里留一个永不释放的订阅者，
// 长时间运行后广播开销持续增长（泄漏）。
func TestSSEUnsubscribesOnClientDisconnect(t *testing.T) {
	probe := newSSEProbe()
	h := sseTestHandler(t, probe)

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	readSSELine(t, resp.Body, 2*time.Second)

	// 确认已订阅。
	waitFor(t, 2*time.Second, func() bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return probe.subs == 1
	}, "handler should have subscribed")

	// 断开：取消请求上下文 + 关闭 body。
	cancel()
	resp.Body.Close()

	// 订阅数应归零（defer cancel() 生效）。
	waitFor(t, 3*time.Second, func() bool {
		probe.mu.Lock()
		defer probe.mu.Unlock()
		return probe.subs == 0
	}, "handler must unsubscribe when the client disconnects")
}

// TestSSESurvivesWithoutEventSource 未注入事件源时端点仍能建立连接（不 500）。
//
// 用途：控制台关了实时推送（或测试环境）时，前端连上来不该报错，
// 只是永远收不到 change 事件——前端另有兜底轮询。
func TestSSESurvivesWithoutEventSource(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	// consoleHandler 不注入 Events → h.cfg.Events 为 nil。

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d want 200 (nil event source must not break the endpoint)", resp.StatusCode)
	}
	line := readSSELine(t, resp.Body, 2*time.Second)
	if !strings.HasPrefix(line, ":") {
		t.Errorf("first frame=%q want connect confirmation", line)
	}
}

// TestSSEWithRealBus 与真实总线联通：池内状态变化 → SSE 事件。
//
// 这是端到端的核心链路验证（pool.markDirty → bus → SSE → 客户端）。
func TestSSEWithRealBus(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	p := pool.New("")
	p.SetChangeNotifier(bus)
	p.Add(&auth.Auth{UID: "u-bus", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "bus"})
	defer p.SetChangeNotifier(nil)

	h := NewHandler(Config{
		Pool:           p,
		APIKey:         "sk-master",
		Events:         bus,
		ConsoleEnabled: true,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, cancel := openSSE(t, srv.URL, "sk-master")
	defer cancel()
	defer resp.Body.Close()
	readSSELine(t, resp.Body, 2*time.Second) // 连接确认

	// 触发一次真实状态变化（额度写入 → markDirty → 总线 → SSE）。
	p.SetCredits("u-bus", 42)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l := readSSELine(t, resp.Body, time.Second)
		if strings.HasPrefix(l, "event:") {
			return // 收到即通过
		}
	}
	t.Error("no SSE event after a real pool state change (end-to-end chain broken)")
}

// --- 测试辅助 ---

// sseTestHandler 构造带 probe 事件源的 handler。
//
// ConsoleEnabled 必须为 true：/admin/api/events 与其余控制台接口同属
// registerAdmin 注册的路由组（关掉控制台就该一起 404）。
func sseTestHandler(t *testing.T, probe *sseProbe) http.Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u-sse", AccessToken: "at", ExpiresAt: 9999999999, Realm: realm.WB, Nickname: "sse"})
	return NewHandler(Config{
		Pool:           p,
		APIKey:         "sk-master",
		Events:         probe,
		ConsoleEnabled: true,
	})
}

// openSSE 建立带鉴权头的 SSE 连接。
//
// 这正是前端必须用 fetch 而非原生 EventSource 的原因：
// 鉴权走 Authorization 头，而原生 EventSource 无法设置请求头。
func openSSE(t *testing.T, base, key string) (*http.Response, func()) {
	t.Helper()
	req, err := http.NewRequest("GET", base+"/admin/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	ctx, cancel := contextWithCancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return resp, cancel
}

// waitFor 轮询 cond 直到为真或超时。
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error(msg)
}

// contextWithCancel 建一个可取消的请求上下文（用于模拟浏览器断开连接）。
func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// sseLineReader 为每个连接维持一个 bufio.Reader。
//
// 必须复用同一个 reader：bufio 会把整块数据读进自己的缓冲，
// 若每次调用都新建，第二行之后就读不到了（缓冲被丢弃）。
type sseLineReader struct {
	mu sync.Mutex
	br *bufio.Reader
}

func newSSELineReader(r io.Reader) *sseLineReader {
	return &sseLineReader{br: bufio.NewReader(r)}
}

func (s *sseLineReader) readLine() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line, err := s.br.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

// sseReaders 把响应体绑定到持久 reader（同一连接多次读行必须复用）。
var sseReaders sync.Map // *http.Response -> *sseLineReader

// readSSELine 读一行 SSE 输出（阻塞到该行结束或超时）。
//
// 超时保护避免端点未输出时测试永久挂起。
func readSSELine(t *testing.T, r io.Reader, timeout time.Duration) string {
	t.Helper()
	v, ok := sseReaders.Load(r)
	var sr *sseLineReader
	if ok {
		sr = v.(*sseLineReader)
	} else {
		sr = newSSELineReader(r)
		sseReaders.Store(r, sr)
	}
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := sr.readLine()
		ch <- res{l, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil && got.line == "" {
			t.Fatalf("read SSE line: %v", got.err)
		}
		return got.line
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for SSE line")
		return ""
	}
}
