package usagelog

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"
)

// 本文件是控制台「使用统计」的**累计口径**后端：回答"到现在为止总共上传/消耗了多少、
// 额度总共被打掉多少"，并按账号 / 模型 / 产品线拆分。
//
// # 与内存环形缓冲（Recent）的分工——这是本文件唯一需要理解的设计决定
//
// 内存环形缓冲是**窗口**口径（容量默认 500 条），清空条数上限由 memory_size 决定；
// 而"累计总量"如果只对缓冲求和，会随着窗口滚动**原地倒退**：日志一直在长，
// 页面上"累计上传 1.2M token"却会在某个时刻变小。这种数字比没有更糟——它看起来
// 是权威账本，实际是个会缩水的窗口和。故累计统计一律直接读**日志文件**：
// 主文件 + 全部备份（正是 SizeInfo.LimitBytes 所约束的那批文件）。
//
// 因此必须说清的**口径边界**（控制台文案与此处注释是同一份约定的两处落地）：
//   - 统计覆盖"当前保留的日志文件"，更早被轮转删除的记录不在其中（旧文档删了就没了）；
//   - 手动 Clear 后归零，因为文件被真的删掉了；
//   - 进程重启后从文件重算，而不是从 0 开始——这正是"持久账本"该有的语义。
//
// # 为什么不给运行时加累计计数器
//
// 计数器省掉扫文件，但会引入一个更难解释的口径："进程启动以来"而不是"日志覆盖范围"。
// 那意味着重启即归零、与页面上同时显示的 total/kept（进程态字段）纠缠在一起，
// 用户看到的两组数字会互相对不上。以文件为唯一事实来源，两组数字同源同义。
//
// # 代价与缓存
//
// 全量扫描的代价与文件大小成正比（上限 = max_size_mb × (1 + max_backups)，默认 64MB），
// 而控制台会按秒级轮询。故按 (logger.gen, logger.total) 做缓存：
//   - total 未变 ⇒ 期间没有任何新记录；
//   - gen 未变   ⇒ 期间没有轮转、没有清空。
//
// 两者同时满足才复用上次结果。任何"可能改变磁盘内容"的事件都会改变其中一个，
// 因此不会出现陈旧数字长期驻留。文件层面的外部篡改（手工编辑）不在契约内——
// 那种情况重启一次就对齐了。
const (
	// summaryGroupLimit 每个维度的最大分组数（超出折叠为 key="其他"）。
	// 为什么要上限：一个跑了几万次调用的实例，按账号分组的键数没有上限，
	// 任其膨胀会让 JSON 响应和控制台表格都失去意义。64 足以覆盖正常规模的部署，
	// 且远小于"折行渲染"的成本线。
	summaryGroupLimit = 64
	// summaryOtherKey 折叠桶的键名。
	summaryOtherKey = "其他"
	// summaryScanBufBytes JSONL 单行的读取缓冲从 64KB 起——
	// 与 maxEntryBytes 同量级。单行超限时 bufio.Scanner 会报 ErrTooLong，
	// 按坏行跳过（一条坏行不该毁掉整份统计）。
	summaryScanBufBytes = 64 << 10
	// summaryMaxScanBufBytes 单行缓冲的硬上限：给到 1MB 冗余，超过即视为脏数据。
	summaryMaxScanBufBytes = 1 << 20
)

// Totals 一组累计指标。所有字段都是**可加的**，这让"按维度分组求和"和
// "整体求和"共用同一套代码，也让前端做分组校验（各行之和不等于合计即说明有 bug）。
type Totals struct {
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CreditsUsed      float64 `json:"credits_used"`
	// CreditsUnknown 成功调用里未能观测到消耗的次数（**额度下界告警**）。
	// 必须与 CreditsUsed 并列暴露：把"未知"折进 0 会让额度统计看起来是完整的，
	// 而实际有一批成功调用的开销根本没算进去（上游没给 credit 且余额差值不可用时就是这种）。
	CreditsUnknown int64 `json:"credits_unknown"`
	// CreditsFailedUnknown 失败调用里同样没有消耗数字的次数。
	// 单列而不是并进 CreditsUnknown：失败请求本就不产生消耗，
	// 混在一起会让"有多少成功调用没记上账"这个真正的告警被失败风暴淹没。
	CreditsFailedUnknown int64 `json:"credits_failed_unknown"`
	// DurationSumMS/TimedCount 是**配对**字段（平均值必须由二者算出）。
	// 单给一个平均 milliseconds 会在分组聚合时无法再加权求和：
	// 两组各 100ms 与 300ms，平均成 200ms 就丢掉了权重信息。
	DurationSumMS int64 `json:"duration_sum_ms"`
	TimedCount    int64 `json:"timed_count"`
	TTFBSumMS     int64 `json:"ttfb_sum_ms"`
	TTFBCount     int64 `json:"ttfb_count"`
	// FirstTime/LastTime 本组覆盖的时间范围（按 Entry.Time）。
	//
	// 用指针而不是 time.Time，是为了让"从未有记录"能在 JSON 里**缺席**：
	// time.Time 是 struct，encoding/json 的 omitempty 对 struct **一律不生效**
	// （isEmptyValue 只处理 array/map/slice/string/bool/数值/指针/接口），
	// 零值会被序列化成 "0001-01-01T00:00:00Z" ——前端 new Date() 照单全收，
	// 于是"一条记录都没有"会显示成「统计范围：01/01 08:00 起」，把不存在的时间当成事实。
	// 指针为 nil 时字段直接缺席（JSON 里是 null），前端才能走"尚无记录"分支。
	FirstTime *time.Time `json:"first_time"`
	LastTime  *time.Time `json:"last_time"`
}

// Add 累加一条记录。**唯一**的记账入口：整体统计与分组统计都走它，
// 避免两处各写一套加法而在某个字段上悄悄分叉。
//
// 状态判定只认 2xx（与 handler 是否算成功一致）；非 2xx 计入 Failed。
// token 直接取记录里的三项（上游未给 usage 时它们本来就是 0）。
func (t *Totals) Add(e Entry) {
	t.Requests++
	if e.Status >= 200 && e.Status < 300 {
		t.Success++
	} else {
		t.Failed++
	}
	t.PromptTokens += int64(e.PromptTokens)
	t.CompletionTokens += int64(e.CompletionTokens)
	t.TotalTokens += int64(e.TotalTokens)
	t.CachedTokens += int64(e.CachedTokens)
	// 消耗数据的口径分三档，不能压成两档（这是一个会误导运维的细节）：
	//   1. 成功（2xx）且观测到消耗 → 计入 CreditsUsed；
	//   2. 成功但没观测到 → 计入 CreditsUnknown：**钱可能花了但没记上**，是真正的"下界告警"，
	//      因为它意味着页面上显示的额度总消耗可能低于实际；
	//   3. 失败（非 2xx）→ 不计入任何消耗类指标。失败请求本来就不产生消耗
	//      （见 internal/server/logging.go：402/429/5xx 一律记 null），把它算进"未记录消耗"
	//      会让一次 429 风暴把告警数字冲到几千，运维再也看不出"到底有多少成功调用没记上账"。
	//      CreditsFailedUnknown 仍然如实记录该档条数，供需要区分的人使用。
	switch {
	case e.CreditsKnown && e.CreditsUsed != nil:
		t.CreditsUsed += *e.CreditsUsed
	case e.Status >= 200 && e.Status < 300:
		t.CreditsUnknown++
	default:
		t.CreditsFailedUnknown++
	}
	// 耗时只在"真被测到"时计入：同步路径可能有 0（亚毫秒），
	// 那仍然是有效观测，故用 >=0 而不是 >0；负值（时钟回拨等脏数据）排除。
	if e.DurationMS >= 0 {
		t.DurationSumMS += e.DurationMS
		t.TimedCount++
	}
	if e.TTFBMS > 0 {
		t.TTFBSumMS += e.TTFBMS
		t.TTFBCount++
	}
	// 时间范围：首条建立下界，后续只推进上界。
	// 不吃 Entry 的时间顺序假设——文件行序理论上等于写入序（追加写），
	// 但手工拼接/跨文件边界时未必，故用比较而非"取首末"。
	//
	// 只在字段尚未有值时取 e.Time（e.Time 零值行会被跳过）：
	// 一旦 FirstTime 已设置，"比它更早"的比较无需再管零值，逻辑更直白。
	if !e.Time.IsZero() {
		if t.FirstTime == nil || e.Time.Before(*t.FirstTime) {
			v := e.Time
			t.FirstTime = &v
		}
		if t.LastTime == nil || e.Time.After(*t.LastTime) {
			v := e.Time
			t.LastTime = &v
		}
	}
}

// Group 一个维度分组（按账号 / 模型 / 产品线）。
type Group struct {
	Key    string `json:"key"`
	Totals Totals `json:"totals"`
}

// Summary 累计统计快照（GET /admin/api/usage 的 summary 字段）。
type Summary struct {
	// Totals 整体合计（内嵌：JSON 里字段直接铺平，前端读 summary.requests 这类路径）。
	Totals
	// ByRealm/ByUID/ByModel 三个维度的明细，已排序（requests 倒序，同值按 key 升序）。
	ByRealm []Group `json:"by_realm"`
	ByUID   []Group `json:"by_uid"`
	ByModel []Group `json:"by_model"`
	// GroupOverflowKeys 三个维度**合计**被折叠进 key="其他" 的键数。
	// 是合计而不是按维度分开：它只用于"有低频键被合并了"这一句提示，
	// 而按维度分开需要把字段改成对象，收益不足以支付契约复杂度。
	// 前端措辞必须与之匹配（说"各维度合计"，不能在某一个页签下声称"本维度有 N 个被合并"）。
	GroupOverflowKeys int `json:"group_overflow_keys"`
	// ScanIncomplete 本次统计**是否因读取失败而不完整**：true = 有日志文件没读完
	// （被杀软/备份工具独占、读盘错误、或文件里存在超过单行缓冲的脏数据）。
	//
	// 为什么必须暴露：不完整的数字在形态上与完整的一模一样，运维只看卡片无法分辨
	// "这台机器真的只用了这么多"和"这次统计少读了一段"。给出这个布尔值，前端才可能
	// 提示"数字可能偏低"。同时它也是后端的正确性保证：不完整的结果**不进缓存**，
	// 否则少算的数字会在没有新流量的窗口里长期驻留（见 Summary 的注释）。
	ScanIncomplete bool `json:"scan_incomplete"`
}

// 顺序是主文件在前、随后 .maxBk → … → .1（备份里越靠后越新）。
//
// 刻意**不依赖**这个顺序做任何时间假设：Totals 的时间范围用比较（min/max），
// 分组用全序排序，故哪份文件先读都不影响结果。文件顺序唯一的作用是
// "先读更可能相关的数据"——除了省一点点内存局部性，它没有任何语义。
// 先做存在性判断而不是交给打开失败：文件可能从未创建（新部署），那是正常状态不是错误。
func (l *Logger) files() []string {
	l.mu.Lock()
	maxBk := l.maxBk
	l.mu.Unlock()
	if l.file == "" {
		return nil
	}
	var out []string
	if _, err := os.Stat(l.file); err == nil {
		out = append(out, l.file)
	}
	for i := maxBk; i >= 1; i-- {
		p := backupPath(l.file, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Summary 返回累计统计快照。
//
// 契约（控制台按此渲染，不得违反）：
//   - nil logger / 未启用 / 未落盘 → 返回**可用但全零**的快照与空数组，绝不 nil：
//     前端拿到 nil 与拿到零值要能区分"没有数据"与"接口坏了"，而这里两者都不算坏。
//   - 只有内存模式（File 为空）时统计恒为零：见下方"为什么不能对内存缓冲求和"的说明。
//   - 任何一次成功返回都会填充缓存；并发调用通过 sumMu 单飞，不会重复扫同一份文件。
func (l *Logger) Summary() Summary {
	if l == nil {
		return emptySummary()
	}
	// 单飞：先到者算，后到者在锁内命中缓存。
	// 锁序固定 sumMu → mu：任何持有 mu 的路径都不得反取 sumMu（本包内不存在这种路径）。
	l.sumMu.Lock()
	defer l.sumMu.Unlock()

	// 缓存元数据（gen/sumCacheGen/sumCacheAt/…）全部在 mu 下读写：
	// 这些字段也会被 Record/Clear/轮转那条路径更新，持锁读取才不会与它们竞争。
	// 判定与取值放在**同一个临界区内**，避免"先判断有效、再读值时已被别人改动"。
	l.mu.Lock()
	if !l.enabled {
		l.mu.Unlock()
		return emptySummary()
	}
	file := l.file
	if file == "" {
		// 只有内存（清空后未落盘 / File 未配置）：无法给出累计口径。
		// 不对内存环形缓冲求和——那会得出一个随窗口滚动而**倒退**的假累计
		// （详见文件头注释）。宁可说明"不可用"，也不给会缩水的数字。
		l.mu.Unlock()
		return emptySummary()
	}
	// 缓存命中：期间没有新记录（total 未变）、没有轮转/清空（gen 未变）。
	if l.gen == l.sumCacheGen && l.total == l.sumCacheTotal && !l.sumCacheAt.IsZero() {
		hit := l.sumCache
		l.mu.Unlock()
		return hit
	}
	// 定格本次统计对应的 (gen, total)：回写缓存时用它们。
	gen, total := l.gen, l.total
	l.mu.Unlock()

	// 扫描在锁**外**进行：一次全量扫描可能要几百毫秒（文件上限 64MB），
	// 持着 mu 会把所有请求出口的 Record 一起堵住。sumMu 已经保证同一时刻
	// 只有一个扫描者，故这里的并发面只有"扫描期间又有新记录/轮转"。
	sum, complete := scanSummary(l.files())
	sum.ScanIncomplete = !complete

	// 只有**完整**扫描才允许写缓存。
	//
	// 为什么不完整结果绝不能缓存：缓存存活条件只看 (gen,total)，而这两个键在
	// "没有新记录、没有轮转"的窗口里都不会变——恰好是夜间空闲、限流后长时间无成功调用
	// 这类最需要正确数字的场景。此时一次读盘失败（Windows 上杀软/编辑器/备份工具持有句柄）
	// 会让少算的数字一直留在页面上，直到下一条记录写入才可能被纠正，
	// 而用户完全无从察觉（除了 ScanIncomplete 这个字段，没有任何别的线索）。
	// 不缓存则下一次调用必然重算：读盘失败是瞬时的，多半立刻就好了。
	if complete {
		// 回写缓存。注意回写的是**进入临界区时**读到的 total——扫描期间可能又有新记录写入，
		// 那条记录不在本次结果里；把当时的 total 记下来，下次调用 total 已变、自然会重算，
		// 不会出现"漏了一条却永远命中缓存"。
		l.mu.Lock()
		l.sumCache, l.sumCacheAt, l.sumCacheTotal, l.sumCacheGen = sum, time.Now(), total, gen
		l.mu.Unlock()
	}
	return sum
}

// emptySummary 全零 + 空数组（而非 nil 切片）：JSON 里是 [] 而不是 null，
// 前端无需特判 `|| []` 就能直接遍历。
func emptySummary() Summary {
	return Summary{
		ByRealm: []Group{},
		ByUID:   []Group{},
		ByModel: []Group{},
	}
}

// scanSummary 扫描给定文件并汇总。files 为空（未落盘）时返回全零 + complete=true。
//
// 第二个返回值 complete 表示"这次统计是否完整"：所有文件都读完了且中途没有
// 超出单行缓冲（见 scanFile）。**调用方必须据此决定要不要缓存结果**——
// 不完整的数字一旦进缓存就会在无流量的窗口里长期驻留（见 Summary 的说明）。
func scanSummary(files []string) (Summary, bool) {
	sum := emptySummary()
	if len(files) == 0 {
		return sum, true
	}
	// 三个维度各用自己的累加器：key → *Totals。
	// 边走边累加（而不是先把 Entry 全收集起来再分组），让内存占用只与**分组数**
	// 成正比，与文件里有多少条记录无关。
	realms := map[string]*bucket{}
	uids := map[string]*bucket{}
	models := map[string]*bucket{}
	complete := true

	// 一条记录的记账入口。三个维度各 Add 一次：字段完全一致，
	// 这正是"分组求和 == 合计"这条不变量的来源。
	parse := func(e Entry) {
		sum.Add(e)
		get(realms, groupingKey(e.Realm)).Add(e)
		get(uids, groupingKey(e.UID)).Add(e)
		get(models, groupingKey(e.Model)).Add(e)
	}

	for _, fp := range files {
		if !scanFile(fp, parse) {
			complete = false
		}
	}

	sum.ByRealm, sum.GroupOverflowKeys = finishGroups(realms, sum.GroupOverflowKeys)
	byUID, over := finishGroups(uids, sum.GroupOverflowKeys)
	sum.ByUID = byUID
	byModel, over2 := finishGroups(models, over)
	sum.ByModel = byModel
	sum.GroupOverflowKeys = over2
	return sum, complete
}

// scanFile 逐行解析一个 JSONL 文件；单条解析失败的行只跳过它自己。
//
// 返回值 complete 表示本次读取**是否完整**：false = 打开失败，或中途因单行超长
// （ErrTooLong）而提前终止。调用方必须把 false 传上去并**放弃缓存结果**——
// 一个残缺的数字若被缓存，会在"没有新记录"的窗口里长期留在页面上（见 Summary）。
//
// 为什么用 bufio.Scanner 而不是一次性 ReadFile：单个日志文件上限 32MB（默认），
// 全部读进内存是浪费；流式读取的内存上界只与单行大小有关。
// 单行超过 summaryMaxScanBufBytes 说明文件已被外部破坏（正常单条上限 64KB），
// 此时保留已解析的部分——残缺但真实的统计，好过为了几行脏数据丢掉整份账本；
// 但**如实标记为不完整**，不假装读完了。
func scanFile(path string, parse func(Entry)) bool {
	f, err := os.Open(path)
	if err != nil {
		// 读不到（权限/被杀软或备份工具独占/文件刚被删）：跳过该文件并标记不完整。
		// 不返回错误中断整次统计——其余文件仍然是有效观测。
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, summaryScanBufBytes), summaryMaxScanBufBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // 坏行/截断行跳过，不影响其余记录
		}
		parse(e)
	}
	// ErrTooLong 与读盘错误都让这次统计不完整；EOF 正常结束才算完整。
	return sc.Err() == nil
}

// finishGroups 把累加器整理成有序输出，并在超过上限时折叠为"其他"。
//
// 排序规则：requests 倒序（谁用得最多先看谁），同值按 key 升序——
// 后者是为了**稳定**：map 迭代顺序随机，不指定次级序会让同一份数据每次刷新
// 表格行序都在跳。折叠桶恒定排在最后。
func finishGroups(m map[string]*bucket, overflow int) ([]Group, int) {
	out := make([]Group, 0, len(m))
	for k, b := range m {
		out = append(out, Group{Key: k, Totals: b.totals})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Totals.Requests != out[j].Totals.Requests {
			return out[i].Totals.Requests > out[j].Totals.Requests
		}
		return out[i].Key < out[j].Key
	})
	if len(out) <= summaryGroupLimit {
		return out, overflow
	}
	// 保留前 summaryGroupLimit 个，其余折叠进"其他"。
	// 被折叠的**键数**计入 overflow，让用户知道有一批低活跃账号被合并了
	// （它们的调用次数和额度仍然计入合计与"其他"行，不丢账）。
	head := out[:summaryGroupLimit]
	rest := out[summaryGroupLimit:]
	other := Group{Key: summaryOtherKey}
	for _, g := range rest {
		other.Totals.AddTotals(g.Totals)
	}
	return append(head, other), overflow + len(rest)
}

// AddTotals 把另一组累计指标合并进本组。
//
// 与 Add(Entry) 并存而不是复用它：Entry 是**原始记录**，而合并是**已被汇总过的**数据。
// 时间范围在这里取并集（不是相加），两个标量字段（token/credits）才是求和。
func (t *Totals) AddTotals(o Totals) {
	t.Requests += o.Requests
	t.Success += o.Success
	t.Failed += o.Failed
	t.PromptTokens += o.PromptTokens
	t.CompletionTokens += o.CompletionTokens
	t.TotalTokens += o.TotalTokens
	t.CachedTokens += o.CachedTokens
	t.CreditsUsed += o.CreditsUsed
	t.CreditsUnknown += o.CreditsUnknown
	t.CreditsFailedUnknown += o.CreditsFailedUnknown
	t.DurationSumMS += o.DurationSumMS
	t.TimedCount += o.TimedCount
	t.TTFBSumMS += o.TTFBSumMS
	t.TTFBCount += o.TTFBCount
	// 时间范围取**并集**（不是相加）：组内最小值再取组间最小值 = 整体最小值，与
	// 逐条 Add 的结果一致——这是"分组求和 == 合计"这条不变量在时间字段上的延续。
	if o.FirstTime != nil && (t.FirstTime == nil || o.FirstTime.Before(*t.FirstTime)) {
		v := *o.FirstTime
		t.FirstTime = &v
	}
	if o.LastTime != nil && (t.LastTime == nil || o.LastTime.After(*t.LastTime)) {
		v := *o.LastTime
		t.LastTime = &v
	}
}

// bucket 分组累加器。定义在包级而不是 scanSummary 内部：finishGroups 需要接收
// 同一类型的 map，而函数内定义的类型无法作为另一个函数的参数类型。
type bucket struct{ totals Totals }

// get 取（必要时新建）某维度下某个键的累加器。
//
// 空键必须显式命名：留空会让控制台出现一行没有名字的分组，看起来像渲染 bug，
// 而它其实是"凭证未标注产品线 / 上游未返回模型名"的真实状态。
func get(m map[string]*bucket, key string) *Totals {
	if key == "" {
		key = "未标注"
	}
	b := m[key]
	if b == nil {
		b = &bucket{}
		m[key] = b
	}
	return &b.totals
}

// groupingKey 归一化分组键：只做"去除首尾空白"，不做截断或大小写折叠。
//
// 为什么不截断 uid：不同账号完全可能共享前 8 位，按截断值分组会把它们的消耗
// 合并到一行（账目对不上，且看不出来）。截断是纯展示问题，交给前端。
func groupingKey(s string) string { return strings.TrimSpace(s) }
