// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/usagelog"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	runtime *RuntimeConfig
	start   time.Time
	seq     int64 // 使用日志序号（与 stdout 表格日志的 chatSeq 独立）
	model   string
	mode    string // "stream" | "sync"
	uid     string // 完整 uid，展示时只取前 8 位
	ttfb    time.Duration
	toks    int // <0 表示 usage 缺失 → 显示 "-"
	status  int
	// usage 扩展（使用日志用）：上游 usage 缺失时 hasUse=false。
	prompt   int
	cached   int
	hasUse   bool
	usageRaw map[string]any

	// 积分。两条来源，优先级从高到低：
	//  1. creditFromUsage：上游 usage.credit 直接报告的本次消耗（实测可用，精确到小数）。
	//     这是**首选**——上游自己算的账，无需差值推断，也省掉一次余额查询往返。
	//  2. credBefore/credAfter 差值：仅当上游没给 credit 时的兜底（其他上游实现可能不返回）。
	// credKnown=false 表示两条路径都没拿到，此时积分记 null 而非 0。
	creditFromUsage float64
	hasCreditUsage  bool
	credBefore      int64
	credAfter       int64
	credKnown       bool
	balErr          string
	// poolCreditsAfter 本地扣减/校准后池内的余额（供日志记录"扣减后额度"）。
	// 与 CreditsAfter（上游权威快照）不同：这是**含本地扣减**的即时值。
	poolCreditsAfter *int64
	// calibErr 单号校准失败原因（非空时该号的权威基准未更新，本地估算继续生效）。
	calibErr string

	realm string // 请求归属的产品线
	nname string // 账号昵称（日志可读性用）

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRow(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.status, s.toks)
}

// finish 请求出口的统一收尾：先落 stdout 表格日志，再做使用日志。
//
// 积分来源优先级（关键，曾因只做差值法而统计恒为 0）：
//
//  1. **上游 usage.credit**（首选）——上游自己算的账，精确到 0.01。
//     实测 ai 线每次调用都返回（如 "credit":10.85）。有它时**完全跳过余额查询**：
//     既准确又省掉一次上游往返。
//  2. **余额差值兜底**——仅当上游没给 credit 时（其他上游实现可能不返回）。
//     局限：before 取"请求出口时刻"的池内缓存值，同账号并发在途时可能被重复计入。
//
// 关于耗时：差值兜底会发一次余额查询，而 Go 的 net/http 对**非流式**响应体有 ~2KB
// 缓冲，小于该阈值的响应会等 handler 返回才真正 flush——所以那次查询用专用短超时
// （main.go 注入，20s），宁可快速失败也不占住连接。走 credit 路径时不存在这个问题。
func (s *chatStat) finish(h *Handler, pl *pool.Pool) {
	// 先定格请求耗时：必须在余额查询**之前**取，否则 duration_ms 会被
	// 那次额外查询的 RTT 污染（表格日志里的 total 是干净的，两者会对不上）。
	elapsed := time.Since(s.start)
	s.done()
	if pl != nil && s.hasCreditUsage && s.status >= 200 && s.status < 300 {
		_, model, _ := realm.SplitChannelPrefix(s.model, h.channelPrefixes())
		pl.NoteModelCharge(s.uid, s.realm, model, s.creditFromUsage)
	}
	if !h.cfg.UsageLog.Enabled() {
		if pl != nil && pl.CreditFloor() > 0 && s.hasCreditUsage && s.status >= 200 && s.status < 300 {
			pl.DeductCredits(s.uid, s.creditFromUsage)
		}
		return
	}
	// 只有真正落到某个账号上的请求才有积分可言（503 全不可用、413 等没有 uid）。
	if s.uid != "" {
		switch {
		case s.hasCreditUsage && s.status >= 200 && s.status < 300:
			// 上游已直接报告消耗 → 采信，并且**立即本地扣减**：
			// 展示与选号排序当场反映真实余额，不必等下一次额度刷新。
			// 未成功的请求不扣（上游即便给了 credit 也不代表真扣了钱）。
			s.credKnown = true
			// 顺序至关重要：**先校准，再扣减**。
			//
			// 上游余额快照有计费延迟（实测"调用前后差值恒为 0"即其证据）——
			// 它反映的是"本次消耗尚未落账"的状态。故正确做法是：
			//   1. 先取权威快照作为基准（若已到校准时间）；
			//   2. 再减掉本次已知消耗。
			// 反过来（先扣后校准）会让快照把刚扣掉的消耗又覆盖回来，
			// 展示额度不降反升，等于整套本地扣减白做。
			// 附带好处：余额未知的账号先由校准建立基准，本次扣减才能生效
			// （DeductCredits 对未知余额拒绝扣减）。
			s.maybeCalibrate(h, pl)
			if pl != nil {
				if after, ok := pl.DeductCredits(s.uid, s.creditFromUsage); ok {
					// 记扣减值而非校准值：校准值只是"本次消耗尚未落账"的基准，
					// 扣减后的值才是这个账号**当前**可用的真实额度。
					s.poolCreditsAfter = &after
				}
			}
		case s.runtimeFor(h).RefreshBalanceAfter != nil:
			s.balanceDiffFallback(h, pl)
		}
	}
	h.cfg.UsageLog.Record(s.usageEntry(elapsed))
	// 用量日志不在池的状态变更路径上（池只管额度/冷却/在途/冻结），
	// 单独喊一声：控制台的「使用日志」面板才能跟着实时刷新，而不是等轮询。
	h.notifyState()
}

// maybeCalibrate 按需对**当前账号**做一次权威余额校准（单号刷新，非全量）。
//
// 目的：本地扣减是累加近似（小数进位 + 上游口径假设），会缓慢漂移。
// 单号校准用上游绝对余额把这个号拉回权威值，误差不累积。
//
// 为什么按 interval 节流而非每次请求都查：连续调用同一账号时，
// 每次多打一次余额查询纯属浪费——本地扣减已经把余额维护得足够准，
// 校准只是防漂移的保险丝。interval<=0 时退化为每次都校准（供需要绝对精确的场景）。
func (s *chatStat) maybeCalibrate(h *Handler, pl *pool.Pool) {
	runtime := s.runtimeFor(h)
	if runtime.RefreshBalanceAfter == nil || pl == nil {
		return
	}
	if !pl.CalibrateDue(s.uid, runtime.CalibrateInterval) {
		return
	}
	// 先标记再查询：查询失败也应当节流，否则上游持续报错时会对每个请求重试。
	pl.MarkCalibrated(s.uid)
	after, err := runtime.RefreshBalanceAfter(pl, s.uid)
	if err != nil {
		// 校准失败不影响本次记账：本地扣减的值已经记进日志，只是没能拉回权威基准。
		s.calibErr = err.Error()
		return
	}
	s.poolCreditsAfter = &after
}

// balanceDiffFallback 兜底路径：调用前后余额差值。
// 仅在"上游没给 usage.credit"时才会走到（见 finish 的优先级说明）。
//
// 只对**成功**请求记差值：失败（402/429/5xx）恰恰是"缓存余额已过期"的高发场景，
// 缓存里是 350、实际早已归零，一减就得出"这次调用吃掉了 350 分"的假数据。
func (s *chatStat) balanceDiffFallback(h *Handler, pl *pool.Pool) {
	// 调用前余额取池内缓存快照，必须在刷新**之前**取值，否则差值恒为 0。
	var before int64
	hadBaseline := false
	if pl != nil && pl.CreditsKnownOf(s.uid) {
		if v, ok := pl.CreditsOf(s.uid); ok {
			before, hadBaseline = v, true
		}
	}
	after, err := s.runtimeFor(h).RefreshBalanceAfter(pl, s.uid)
	// 兜底路径已经查过一次权威余额 → 记一次校准时刻（pl 为 nil 时跳过）。
	// 否则"上游不给 credit"的账号会每次请求都查余额（等于没有节流），
	// 而 credit 路径的 maybeCalibrate 仍会按 interval 再查一次，双重浪费。
	if pl != nil {
		pl.MarkCalibrated(s.uid)
	}
	switch {
	case err != nil:
		s.balErr = err.Error()
	case !hadBaseline:
		// 冷启动首次：无基准可减，本次积分为 null。
		// 刷新结果已由回调写回池，下一次请求即走 hadBaseline 分支。
	case s.status < 200 || s.status >= 300:
		// 请求未成功：本次消耗不可归因，记 null（基准已被刷新更新）。
	default:
		s.credBefore, s.credAfter, s.credKnown = before, after, true
	}
}

// usageEntry 把本次请求的最终统计组装成一条使用日志。
// elapsed 由调用方在余额查询之前定格（否则会被该查询的 RTT 污染）。
// 只有在 usage 存在时 token 三字段才有意义；积分仅在 credKnown 时给出差值。
func (s *chatStat) usageEntry(elapsed time.Duration) usagelog.Entry {
	e := usagelog.Entry{
		Seq:        s.seq,
		Time:       s.start,
		Realm:      s.realm,
		UID:        s.uid,
		UID8:       uidPrefix(s.uid),
		Nickname:   s.nname,
		Model:      s.model,
		Mode:       s.mode,
		Status:     s.status,
		DurationMS: elapsed.Milliseconds(),
		TTFBMS:     s.ttfb.Milliseconds(),
	}
	// 时间取请求**开始**时刻：与"这条日志属于哪次调用"的直觉一致，
	// 也避免长流式请求的结束时刻漂移到下一条日志之后。
	if s.hasUse {
		e.PromptTokens = s.prompt
		e.CompletionTokens = s.toks
		e.TotalTokens = s.prompt + s.toks
		e.CachedTokens = s.cached
		e.Usage = s.usageRaw
	}
	// 积分：上游 credit 优先，差值兜底（见 finish 的优先级说明）。
	if s.hasCreditUsage {
		used := s.creditFromUsage
		e.CreditsUsed = &used
		e.CreditsKnown = true
		e.CreditsSource = "usage"
		// 附带"扣减后池内余额"：日志一眼可见该号当前额度，无需另查控制台。
		e.PoolCreditsAfter = s.poolCreditsAfter
		e.CalibrateError = s.calibErr
	} else if s.credKnown {
		// 兜底路径：给的是整数余额快照，转成消耗值。
		before, after := s.credBefore, s.credAfter
		used := float64(before - after)
		e.CreditsBefore = &before
		e.CreditsAfter = &after
		e.CreditsUsed = &used
		e.CreditsKnown = true
		e.CreditsSource = "balance_diff"
	}
	if s.balErr != "" {
		e.BalanceError = s.balErr
	}
	return e
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	r        io.Reader
	start    time.Time
	ttfb     time.Duration
	seen     bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage bool // 末帧是否带 usage
	tokens   int
	prompt   int            // usage.prompt_tokens
	cached   int            // 缓存命中 token（上游报告时，字段名多路探测）
	raw      map[string]any // 末帧 usage 原样副本
	credit   float64        // usage.credit 报告的本次积分消耗
	hasCred  bool           // 末帧是否带可用的 credit
	line     []byte
	event    strings.Builder
	skipLine bool
	done     bool
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{r: r, start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Prompt 返回 usage.prompt_tokens（无 usage 时为 0）。
func (s *chatStatsReader) Prompt() int { return s.prompt }

// Cached 返回缓存命中 token 数（上游不报告时为 0）。
func (s *chatStatsReader) Cached() int { return s.cached }

// Raw 返回末帧 usage 的原样副本（无 usage 时为 nil）。
func (s *chatStatsReader) Raw() map[string]any { return s.raw }

// Credit 返回末帧 usage.credit（上游报告的本次积分消耗）与是否可用。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCred }

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信其 token 三项。
// usage 缺失时不覆盖已解析的值——上游通常只在末帧带 usage，中间帧没有。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	line = strings.TrimPrefix(line, "\uFEFF")
	if s.done {
		return
	}
	if line == "" {
		s.parseSSEUsage()
		return
	}
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
	if payload == "[DONE]" {
		s.parseSSEUsage()
		s.done = true
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	// Bound observation without hiding errors from the API's SSE parser.
	if s.event.Len()+len(payload)+1 > 16<<20 {
		s.event.Reset()
		return
	}
	s.event.WriteString(payload)
	s.event.WriteByte('\n')
}

func (s *chatStatsReader) parseSSEUsage() {
	payload := s.event.String()
	s.event.Reset()
	var chunk struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.raw = chunk.Usage
	s.tokens = intOf(chunk.Usage, "completion_tokens")
	s.prompt = intOf(chunk.Usage, "prompt_tokens")
	s.cached = cachedTokensOf(chunk.Usage)
	// 积分消耗：上游直接报告时优先采信（比余额差值可靠，见 finish 的优先级说明）。
	if c, ok := usagelog.CreditOf(chunk.Usage); ok {
		s.credit, s.hasCred = c, true
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, err := s.r.Read(p)
	data := p[:n]
	for len(data) > 0 && !s.done {
		end := bytes.IndexByte(data, '\n')
		count := len(data)
		if end >= 0 {
			count = end + 1
		}
		if !s.skipLine {
			if len(s.line)+count > 16<<20 {
				s.line = nil
				s.skipLine = true
			} else {
				s.line = append(s.line, data[:count]...)
			}
		}
		data = data[count:]
		if end >= 0 {
			if !s.skipLine {
				s.parseSSELine(string(s.line))
			}
			s.line = s.line[:0]
			s.skipLine = false
		}
	}
	if err != nil {
		if !s.skipLine && len(s.line) > 0 {
			s.parseSSELine(string(s.line))
		}
		s.line = nil
		s.parseSSEUsage()
	}
	return n, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// intOf 从 usage map 取一个整数 token 字段；缺失/非数值返回 0。
// JSON 数字经 encoding/json 解析一律为 float64，故只认 float64（与既有 completionTokens 口径一致）。
func intOf(u map[string]any, key string) int {
	v, ok := u[key].(float64)
	if !ok {
		return 0
	}
	return int(v)
}

// cachedTokensOf 提取"缓存命中"token 数。
//
// 上游字段名不由本项目定义，且 OpenAI 系与 Anthropic 系的叫法完全不同，
// 故按从具体到宽泛的顺序多路探测；都没命中则返回 0（表示上游未报告缓存命中，
// 而不是"命中 0 个 token"——这两者在语义上确实不同，但都只能记 0，
// 完整的 usage 原样副本已随 Entry.Usage 落盘，需要精确区分时可回溯原始字段）。
func cachedTokensOf(u map[string]any) int {
	// OpenAI 风格：prompt_tokens_details.cached_tokens
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if n := intOf(d, "cached_tokens"); n > 0 {
			return n
		}
	}
	// 其余常见拼法（部分兼容网关/上游直出）。
	for _, k := range []string{
		"cached_tokens",
		"prompt_cache_hit_tokens",
		"cache_read_input_tokens",
	} {
		if n := intOf(u, k); n > 0 {
			return n
		}
	}
	return 0
}

// usageRawOf 从 Aggregate 响应里取 usage 原样副本（无则 nil）。
func usageRawOf(resp map[string]any) map[string]any {
	u, _ := resp["usage"].(map[string]any)
	return u
}

// promptTokens 从 Aggregate 响应提取 usage.prompt_tokens；缺失返回 0。
func promptTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return 0
	}
	return intOf(u, "prompt_tokens")
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
func uidPrefix(uid string) string {
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
// toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid string, status int, toks int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	if len(model) > 11 {
		model = model[:11]
	}
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1f", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | uid=%s | TTFB=%s | tok=%s | %stok/s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		uidPrefix(uid),
		ttfbMS,
		tokField,
		tokpsField,
		total.Seconds(),
	)
}
