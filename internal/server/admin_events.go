// admin_events.go 控制台实时推送端点（SSE）。
//
// # 事件带状态负载：让"推"取代"拉"
//
// 事件帧形如：
//
//	event: change
//	data: {"overview":{…},"usage":{…}}    // usage 仅在用量有变化时附带
//
// 前端收到后**直接应用**这份快照，不再回拉 HTTP：
//   - 端到端延迟从"两次往返"降到"一帧"，且页面不再受轮询相位影响；
//   - overview 复用 /admin/api/overview 的渲染路径（见 sseFrame/captureJSON），
//     于是"推"与"拉"的口径在构造上一致，加字段不会漏；
//   - usage 单独限流（约 1s）：overview 约 6KB，200 条用量明细约 50KB，
//     账号状态该逐次推，用量面板晚一秒无感。
//
// 前端保留两条"拉"：**重连后补一次全量**（信号会被总线的合并窗口丢弃，断线期间
// 的变化只能靠这次补齐）与手动刷新按钮。
//
// # 为什么不用浏览器原生 EventSource
//
// 原生 EventSource 无法携带自定义请求头，而本项目的控制台鉴权是
// `Authorization: Bearer <api_key>`。若为 SSE 单独开一个 `?token=` 查询参数，
// API Key 会进入浏览器历史、反向代理访问日志、Referer 头——**明文泄漏密钥**，
// 不可接受。所以前端改用 fetch + ReadableStream 手动解析 SSE（见 console.html），
// 可以正常带 Authorization 头。
//
// # 部署约束（重要，写在代码里防回归）
//
// 本端点依赖**长连接**，因此 `http.Server` 绝不能设置 `WriteTimeout`：
// 一旦设置，连接会在超时后被强制掐断，表现为前端反复重连但永远收不到数据。
// 当前 main.go 只设了 ReadHeaderTimeout，是刻意为之。
// 另外若有反向代理，必须关闭响应缓冲（Nginx: `proxy_buffering off`），
// 否则事件会被攒在代理层不发。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseHeartbeat 心跳间隔。
//
// 为什么必须发心跳：SSE 连接会经过各式中间设备（代理、NAT、云负载均衡），
// 静默连接常被单方面回收，而客户端在 TCP FIN 到达前察觉不到（表现为"实时更新
// 突然不动了"）。定期发一个 SSE 注释行（以 ':' 开头，客户端会忽略）即可保活。
//
// 25 秒：短于常见的 30~60 秒空闲超时，同时心跳流量可忽略。
const sseHeartbeat = 25 * time.Second

// adminEvents 状态变化推送（SSE 长连接）。
//
// 连接生命周期：客户端连上后一直保持，直到客户端断开、服务端关闭、
// 或总线被关闭（进程退出）。每条事件都是"去取最新状态"的信号。

// EventSource 状态变化事件源（由 *eventbus.Bus 实现；nil 接收者亦安全）。
//
// 只暴露 Subscribe：handler 不关心总线怎么合并、怎么广播，
// 只需要"给我一个会在状态变化时收到信号的通道"。
type EventSource interface {
	// Subscribe 返回信号通道与取消函数。通道关闭表示事件源已终止
	// （进程退出），前端应重连。
	Subscribe() (<-chan struct{}, func())
}

func (h *Handler) adminEvents(w http.ResponseWriter, r *http.Request) {
	// 必须先确认底层支持流式写出（httptest 的 recorder 也实现了 Flusher，
	// 所以测试可用；但若将来挂了不支持 Flush 的中间件，这里会明确报错
	// 而不是静默地攒着不发）。
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "streaming unsupported: response writer does not implement http.Flusher",
		})
		return
	}

	// 订阅状态变化信号。
	//
	// nil 守卫是必需的：Events 是接口字段，未注入时它的零值是 nil 接口，
	// 直接调用方法会 panic（nil 接口没有方法表）。这在"控制台开着但实时推送
	// 未接线"的部署下真会发生，不是理论情况。
	//
	// 未接线时 signals 为 nil 通道：select 中该分支永久阻塞（Go 语义），
	// 于是连接保持打开、只发心跳、永不推 change——正是我们要的降级行为。
	var signals <-chan struct{}
	if h.cfg.Events != nil {
		var cancel func()
		signals, cancel = h.cfg.Events.Subscribe()
		if cancel != nil {
			defer cancel()
		}
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Connection", "keep-alive")
	// 关闭代理缓冲：Nginx 等默认会缓冲响应，导致事件不实时。
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// 先发一个注释行，让客户端与中间设备立刻确认流已建立
	// （不依赖任何事件到达，避免"连上了但看不出有没有生效"）。
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// 客户端断开（关标签页/网络断）：正常退出，不是错误。
			return
		case <-heartbeat.C:
			// SSE 注释行：客户端忽略，仅用于保活。
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case _, open := <-signals:
			if !open {
				// 总线关闭（进程退出）：发个事件让前端知道该重连，
				// 然后正常结束——比静默断流更容易排查。
				fmt.Fprint(w, "event: bye\ndata: {}\n\n")
				fl.Flush()
				return
			}
			// 事件带状态负载：前端直接应用，无需再回拉（见文件头说明）。
			if raw := h.sseFrame(); len(raw) > 0 {
				fmt.Fprintf(w, "event: change\ndata: %s\n\n", raw)
			} else {
				// 构建失败（未接线 / 渲染异常）：退化成原来的"去拉"信号。
				// 前端把空负载当无数据，保留旧值，等下一次变化或重连补齐。
				fmt.Fprint(w, "event: change\ndata: {}\n\n")
			}
			fl.Flush()
		}
	}
}

// --- 推送负载：让事件带状态，前端不再回拉 -------------------------------------

// StateNotifier 报告"状态有变"（*eventbus.Bus 实现；nil 接收者安全）。
//
// 与 EventSource 分成两个接口是刻意的：Events 只需 Subscribe（测试 fake 好写），
// 而"允许谁喊 Notify"是接线方（main）的决定——不该让订阅者顺带获得广播能力。
type StateNotifier interface {
	Notify()
}

// notifyState 报告状态有变（nil 安全）。
func (h *Handler) notifyState() {
	if h.cfg.Notify != nil {
		h.cfg.Notify.Notify()
	}
}

const (
	// sseSnapshotTTL 快照缓存有效期。总线按订阅者逐个投递信号，N 个标签页会在同一
	// 窗口内各收到一次；缓存让同一窗口只构建一次负载（否则构建成本 × 标签页数）。
	sseSnapshotTTL = 100 * time.Millisecond
	// sseUsageMinInterval 用量明细的最小推送间隔。
	//
	// 为什么单独限流：overview 约 6KB，而 200 条用量明细可达约 50KB。账号状态该逐次
	// 变化都推（那是用户盯着的），用量面板晚一秒无感；但持续高负载下不限流就是每
	// 120ms 推 50KB（约 400KB/s/标签页）。usagelog.Summary() 在有新记录后要重扫日志
	// 文件，限流同时也保护了磁盘。
	sseUsageMinInterval = time.Second
)

// snapshotCache SSE 负载缓存（Handler 内唯一实例，见 Handler.snap）。
type snapshotCache struct {
	mu       sync.Mutex
	at       time.Time
	data     []byte
	usageSig string    // 上次附带用量时的指纹（total/kept）
	usageAt  time.Time // 上次附带用量的时刻
}

// captureWriter 把 handler 的 JSON 响应写进内存（不经过网络）。
type captureWriter struct {
	hdr  http.Header
	buf  bytes.Buffer
	code int
}

func (c *captureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}
func (c *captureWriter) WriteHeader(code int) { c.code = code }
func (c *captureWriter) Write(b []byte) (int, error) {
	return c.buf.Write(b)
}

// captureJSON 跑一次既有 handler，取其 JSON 体。
//
// 为什么复用 handler 而不另写一份"快照构建"：控制台的"拉"（HTTP）与"推"（SSE）
// 必须是**同一份口径**。各写一份必然随时间跑偏（加了字段忘同步，且没人会立刻发现），
// 复用渲染路径让两者在构造上一致——测试里可以逐字节断言。
func (h *Handler) captureJSON(path string, fn http.HandlerFunc) ([]byte, bool) {
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, false
	}
	cw := &captureWriter{}
	fn(cw, req)
	if cw.code != http.StatusOK {
		return nil, false
	}
	// TrimSpace：即便将来 writeJSON 改成 json.Encoder（会带尾换行），也不会把换行
	// 带进 SSE 的 data 行——data 行里出现裸换行会直接撕碎帧。
	body := bytes.TrimSpace(cw.buf.Bytes())
	if len(body) == 0 {
		return nil, false
	}
	return body, true
}

// sseFrame 构建一次 SSE 负载：{"overview":…,"usage":…}。
//
// usage 只在"指纹变化 + 距上次附带超过 sseUsageMinInterval"时带上；
// 前端按是否存在 usage 键决定要不要重绘用量面板。
func (h *Handler) sseFrame() []byte {
	h.snap.mu.Lock()
	if h.snap.data != nil && time.Since(h.snap.at) < sseSnapshotTTL {
		d := h.snap.data
		h.snap.mu.Unlock()
		return d
	}
	h.snap.mu.Unlock()

	overview, ok := h.captureJSON("/admin/api/overview", h.adminOverview)
	if !ok {
		return nil
	}
	payload := map[string]any{"overview": json.RawMessage(overview)}
	if h.usagePushDue() {
		if usage, ok := h.captureJSON("/admin/api/usage", h.adminUsage); ok {
			payload["usage"] = json.RawMessage(usage)
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	h.snap.mu.Lock()
	h.snap.at, h.snap.data = time.Now(), raw
	h.snap.mu.Unlock()
	return raw
}

// usagePushDue 本次是否该附带用量明细（指纹变化 + 过了最小间隔）。
func (h *Handler) usagePushDue() bool {
	if h.cfg.UsageLog == nil {
		return false
	}
	total, kept, _, enabled := h.cfg.UsageLog.Stats()
	if !enabled {
		return false
	}
	sig := fmt.Sprintf("%d/%d", total, kept)
	h.snap.mu.Lock()
	defer h.snap.mu.Unlock()
	if sig == h.snap.usageSig {
		return false
	}
	if !h.snap.usageAt.IsZero() && time.Since(h.snap.usageAt) < sseUsageMinInterval {
		return false
	}
	h.snap.usageSig, h.snap.usageAt = sig, time.Now()
	return true
}
