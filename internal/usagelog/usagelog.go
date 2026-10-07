// Package usagelog 调用级使用日志：每次 /v1/chat/completions 结束后记一条，
// 回答"这次调用用了哪个凭证、打掉多少积分、输入/输出/缓存命中各多少"。
//
// 与既有 stdout 表格日志（internal/server/logging.go）的分工：
//   - stdout 表格日志是**给人看的实时观测**，一行流式刷屏，不落盘、不结构化；
//   - 本包是**可查询的结构化账本**：内存环形缓冲供控制台近实时查看，
//     JSONL 文件供离线统计（谁打光了额度、缓存命中率变化趋势）。
//
// 积分口径（本项目最重要的一个约定）：上游**不按请求返回消耗**，
// 余额只有绝对快照一个来源，所以"本次消耗"只能算差值：
//
//	消耗 = 调用前余额 - 调用后余额
//
// 调用前余额取池内缓存快照（由额度巡检 / 控制台手动刷新 / 上一次请求后的刷新共同维护），
// 调用后余额在响应写完后向 billing 接口查一次。上游计费本身有延迟时，
// 该差值会把延迟部分计入相邻请求——这是该口径的固有特性，**不做补偿**：
// 补偿需要额外的等待或重试，而观测到什么就记什么更诚实，也更好排查。
//
// 从未观测过余额的账号（creditsKnown=false）只记 null 而不记 0，
// 避免把"未知"伪装成"没消耗"。
//
// # 磁盘占用上限（避免日志爆炸）
//
// 磁盘占用是**有界**的，上限可精确计算：
//
//	最大占用 = max_size_mb × (1 + max_backups)
//
// max_backups=1（默认）→ 2 个文件，32MB 上限时最多 64MB。超出即轮转，
// 最老的备份被删除。三处防线：
//  1. **正常轮转**：单文件超过 max_size_mb → 先删最老备份，再把 .i 后移为 .i+1，最后当前文件 → .1；
//  2. **轮转失败兜底**：改名/删除失败时用 stat **重新校准** fileSize，
//     绝不重置为 0 —— 否则"以为清了但文件还在长"，上限会静默失效；
//  3. **单条记录上限**（maxEntryBytes）：一条记录若因 usage 原样副本过大而超限，
//     丢弃 usage 副本后重写，避免单条撑爆整个日志。
//
// 另外提供 Clear：手动清空内存缓冲 + 落盘文件 + 全部备份（控制台按钮 / DELETE 接口）。
package usagelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Entry 单条调用记录（字段即 JSONL 一行 / 控制台 API 的条目形状）。
// 指针字段用 nil 表达"未知"，与真实的 0 区分开。
type Entry struct {
	Seq        int64     `json:"seq"`
	Time       time.Time `json:"time"`
	Realm      string    `json:"realm"`
	UID        string    `json:"uid"`
	UID8       string    `json:"uid8"`
	Nickname   string    `json:"nickname,omitempty"`
	Model      string    `json:"model"`
	Mode       string    `json:"mode"` // "stream" | "sync"
	Status     int       `json:"status"`
	DurationMS int64     `json:"duration_ms"`
	TTFBMS     int64     `json:"ttfb_ms"`

	// 积分消耗。两条来源，语义见 CreditsSource：
	//   - before/after 为上游余额快照（仅差值兜底路径会填）；
	//   - used 为本次消耗（上游 credit 或差值，取决于来源）；
	//   - credits_known=false 表示两条路径都没拿到，此时 used 为 null。
	//
	// 注意：used 可能是小数（上游 credit 精确到 0.01），故为 float64。
	// 早期版本用 int64 差值，会把这 10.85 这类精确值抹成 0（实测踩过）。
	CreditsBefore *int64   `json:"credits_before"`
	CreditsAfter  *int64   `json:"credits_after"`
	CreditsUsed   *float64 `json:"credits_used"`
	CreditsKnown  bool     `json:"credits_known"`
	// CreditsSource 消耗数据的来源："usage"（上游直接报告，首选）/
	// "balance_diff"（余额差值兜底）/ ""（未知）。区分来源便于排查统计口径。
	CreditsSource string `json:"credits_source,omitempty"`
	// PoolCreditsAfter 本地扣减/校准后的池内余额（即时值，含本地扣减）。
	// 与 CreditsAfter（上游权威快照）不同：它是"用户看到的额度"，
	// 不必等下一次余额刷新就已反映本次消耗。
	PoolCreditsAfter *int64 `json:"pool_credits_after,omitempty"`
	// BalanceError 差值兜底时余额刷新失败的原因。
	BalanceError string `json:"balance_error,omitempty"`
	// CalibrateError 单号校准失败原因（非空时权威基准未更新，本地估算继续生效）。
	CalibrateError string `json:"calibrate_error,omitempty"`

	// token 统计：全部取自上游 usage；上游未给则记 0 且 raw usage 为空。
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens 上游报告的缓存命中 token 数（上游不给则为 0）。
	CachedTokens int `json:"cached_tokens"`

	// Usage 上游 usage 对象的原样副本：字段名不由本项目定义，
	// 留着原始版本才能在发现新的计费/缓存字段时回溯，而不必重放流量。
	// 过大的副本会在落盘前被丢弃（见 maxEntryBytes）。
	Usage map[string]any `json:"usage,omitempty"`
}

// Config 日志器配置（由 cmd/server 从 config.json 的 usage_log 段落注入）。
type Config struct {
	// Enabled 总开关；false 时 Record 为空操作，不占内存不落盘。
	Enabled bool
	// MemorySize 内存环形缓冲容量；不限制控制台读取磁盘历史。<=0 取默认 500。
	MemorySize int
	// MaxSizeMB 单文件大小上限（MB），超过即轮转；<=0 取默认 32。
	// 注意：不存在"不轮转"取值——要放宽就设一个大值，避免误设成无上限。
	MaxSizeMB int
	// MaxBackups 保留的历史备份份数（<file>.1 ... <file>.N）。
	//
	// 零值歧义（易错点）：Go 零值是 0，而"0 份备份"与"没配置"必须区分——
	// 否则任何零值构造 Config 的地方都会静默变成"不留备份"，
	// 与"默认保留 1 份"的意图相反。故用 BackupsSet 显式标记：
	//
	//	BackupsSet=false → 用默认 defaultMaxBackups（1）
	//	BackupsSet=true  → 用本字段的值（含 0 = 显式不留备份）
	MaxBackups int
	// BackupsSet 标记 MaxBackups 是否被显式设置。为 false 时一律用默认值。
	// 这是区分"零值 = 没配置"与"零值 = 不要备份"的可靠方式，
	// 避免调用方漏设字段时静默改变磁盘占用上限。
	BackupsSet bool
	// MaxBytes 单文件上限（字节）。>0 时优先于 MaxSizeMB——供测试注入小上限以覆盖
	// 轮转路径（MaxSizeMB 的最小粒度是 1MB，靠真实数据触发太重）。生产不设。
	MaxBytes int64
	// File JSONL 落盘路径；空 = 只留内存（测试用）。
	File string
}

const (
	// defaultMemorySize 控制台默认可见的历史条数。
	defaultMemorySize = 500
	// defaultMaxSizeMB 单文件默认上限。
	defaultMaxSizeMB = 32
	// defaultMaxBackups 默认保留的备份份数（磁盘占用上限 = 2× 单文件上限）。
	defaultMaxBackups = 1
	// maxEntryBytes 单条记录序列化后的上限。超限时丢弃 Usage 原样副本重写：
	// 上游 usage 的字段集不受本项目控制，极端情况下单条可能很大，
	// 若一条就超过整个文件上限，轮转会陷入"每写一条就轮转"的死循环。
	maxEntryBytes = 64 << 10
	// clearScanBackups Clear 时扫描删除备份的上限份数：
	// 比 maxBackups 宽，用于清掉"曾经配过更大 max_backups"遗留下来的旧文件。
	clearScanBackups = 64
)

// Logger 使用日志器。并发安全（每个请求出口调用一次 Record）。
type Logger struct {
	mu           sync.Mutex
	ring         []Entry
	next         int  // 环形缓冲的下一个写入位置
	filled       bool // 缓冲是否已写满一轮（决定 Recent 的读取顺序）
	total        int64
	file         string
	size         int // 环形缓冲容量
	maxBytes     int64
	maxBk        int   // 保留的备份份数
	fileSize     int64 // 主文件当前字节数（轮转/重启时校准）
	enabled      bool
	pruneBackups int // Obsolete backups are removed on the next write, never during configuration commit.
	rescanSize   bool

	// clearedAt 最近一次 Clear 的时刻（控制台显示"上次清空"）。
	clearedAt time.Time
	// lastErr 最近一次落盘/轮转失败的原因（控制台据此提示"上限可能失效"）。
	// 只在持锁下读写。
	lastErr string

	// gen 文件世代号：任何"让既有累计口径作废"的事件（轮转、清空）都 +1。
	// 统计缓存据此判定失效，而不必比对文件 inode/mtime —— 世代号单调递增，
	// 一次自增同时覆盖"主文件被搬走成备份"与"历史被抹掉"两种语义。
	gen int64
	// sumMu 统计重算的单飞锁：并发请求串行化，先到的算完写缓存，
	// 后到的在锁内直接命中缓存，不会对同一份文件重复扫描。
	// 锁序固定为 sumMu → mu（summary.go 只按此顺序取锁）。
	sumMu sync.Mutex
	// sumCache/sumCacheAt/sumCacheTotal/sumCacheGen 统计缓存与其有效性依据：
	// 仅当 (gen, total) 都没变时命中——期间没有任何新记录、也没有轮转/清空。
	sumCache      Summary
	sumCacheAt    time.Time
	sumCacheTotal int64
	sumCacheGen   int64

	historyMu         sync.Mutex
	historyCache      []Entry
	historyCacheTotal int64
	historyCacheGen   int64
}

// New 构建日志器。File 所在目录不存在时尝试创建；创建失败不致命——
// 降级为纯内存模式（控制台仍可用，只是不落盘），返回的 Logger 正常工作。
func New(cfg Config) *Logger {
	size := cfg.MemorySize
	if size <= 0 {
		size = defaultMemorySize
	}
	mb := cfg.MaxSizeMB
	if mb <= 0 {
		mb = defaultMaxSizeMB
	}
	// 备份份数：BackupsSet=false 一律用默认值（避免零值 = "没配置"被误读成"不要备份"）。
	bk := defaultMaxBackups
	if cfg.BackupsSet {
		bk = cfg.MaxBackups
		if bk < 0 {
			bk = 0 // 显式负值也归一为"不留备份"，不做惩罚性解读
		}
	}
	l := &Logger{
		ring:     make([]Entry, size),
		size:     size,
		file:     cfg.File,
		enabled:  cfg.Enabled,
		maxBytes: cfg.MaxBytes,
		maxBk:    bk,
	}
	if l.maxBytes <= 0 {
		l.maxBytes = int64(mb) << 20
	}
	if !l.enabled || l.file == "" {
		return l
	}
	if dir := filepath.Dir(l.file); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if fi, err := os.Stat(l.file); err == nil {
		l.fileSize = fi.Size()
	}
	// 回填历史：从文件尾部读回最近 size 条塞进环形缓冲。
	// 没有这一步，重启后 Recent() 返回空数组，控制台「使用日志」一片空白——
	// 而数据明明还在磁盘上（用户可见的"日志存在但不显示"）。
	l.loadTailLocked()
	return l
}

// loadTailLocked 从日志文件尾部回填最近 l.size 条记录到内存环形缓冲（构建期调用，无需持锁）。
//
// 为什么必须做：内存环形缓冲是**进程态**，重启即丢；而 JSONL 文件是持久的。
// 控制台只读内存缓冲（Recent），所以不回填就等于"重启后历史看不见"，
// 尽管磁盘上一条不少——这会让用户以为日志丢了（实测就是这个问题）。
//
// 为什么只读**尾部**且只读 size 条：环形缓冲容量就是控制台可见上限，
// 多读无益；而日志文件上限可达 8MB（含备份 16MB），全量解析既慢又占内存。
// 实现从文件末尾按块反向扫描，凑够 size 条即停——代价与"要取的条数"成正比，
// 与文件总大小无关。
//
// 单条解析失败（截断的最后一行 / 手工改坏）一律跳过：历史日志的价值在于
// "能看到发生过什么"，不该因为一行坏数据就整块放弃。
func (l *Logger) loadTailLocked() {
	f, err := os.Open(l.file)
	if err != nil {
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return
	}

	const blockSize = 64 << 10 // 每次回读 64KB（远大于单条上限 64KB，保证总能推进）
	var (
		buf   []byte
		pos   = fi.Size()
		want  = l.size
		lines [][]byte
	)
	for pos > 0 && len(lines) < want+1 {
		read := int64(blockSize)
		if read > pos {
			read = pos
		}
		pos -= read
		chunk := make([]byte, read)
		if _, err := f.ReadAt(chunk, pos); err != nil {
			return
		}
		buf = append(chunk, buf...)
		// 立刻归一化（丢掉尾部空元素）后再判断循环条件——
		// 否则结尾换行符产生的空元素会让行数虚高 1，循环提前退出、少读一块，
		// 最终回填的条数比容量少（实测：容量 3 只回填了 2 条）。
		lines = tailLines(buf)
	}
	// 最前面那段可能是被块边界截断的半行。仅当 pos>0（确实还有更早的内容没读）
	// 才丢首行；读到文件头（pos==0）时首行就是完整记录，丢了会少一条。
	if pos > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > want {
		lines = lines[len(lines)-want:]
	}
	n := l.total // 从 0 开始（total 尚未计数）

	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(ln, &e); err != nil {
			continue // 坏行跳过，不影响其余历史
		}
		l.ring[l.next] = e
		l.next = (l.next + 1) % l.size
		if l.next == 0 {
			l.filled = true
		}
		n++
	}
	// total 继续累计历史条数：控制台的"累计条数"才有连续意义，
	// 且 NextSeq（total+1）不会与已回填的 seq 撞号。
	l.total = n
}

// tailLines 按 '\n' 切分并**丢掉空元素**。
//
// 为什么要丢空元素：JSONL 每行以 '\n' 结尾，所以 bytes.Split 的结果末尾
// 必然多出一个空元素（"a\nb\n" → ["a","b",""]）。这个空元素会让行数虚高 1：
// 既让回填循环提前退出（少读一块），又让"取最后 N 条"取到空串。
// 实测踩过——容量 3 只回填 2 条。中间的空行同样丢弃，空行不是记录。
//
// 不叫 splitLines：测试包里已有一个同名的行拆分辅助（string 版），
// 同名会导致同包内重复声明。
func tailLines(buf []byte) [][]byte {
	parts := bytes.Split(buf, []byte{'\n'})
	out := parts[:0] // 原地复用底层数组，避免额外分配
	for _, p := range parts {
		if len(p) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// Enabled 报告日志是否启用（handler 据此跳过余额刷新等额外开销）。
func (l *Logger) Enabled() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabled
}

// Stats 返回累计记录数、当前保留条数与文件路径（供控制台概览行）。
func (l *Logger) Stats() (total, kept int, file string, enabled bool) {
	if l == nil {
		return 0, 0, "", false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int(l.total), l.countLocked(), l.file, l.enabled
}

// SizeInfo 日志磁盘占用快照——让"会不会爆炸"变成可看的数字，而不是靠猜。
type SizeInfo struct {
	FileBytes   int64 `json:"file_bytes"`   // 主文件当前大小
	BackupBytes int64 `json:"backup_bytes"` // 全部备份合计
	TotalBytes  int64 `json:"total_bytes"`  // 合计占用
	// MaxBytes 单文件上限；MaxBackups 保留份数。
	MaxBytes   int64 `json:"max_bytes"`
	MaxBackups int   `json:"max_backups"`
	// LimitBytes 磁盘占用上限 = MaxBytes × (1 + MaxBackups)。
	// 0 表示"不落盘"（File 为空，只留内存），此时占用恒为 0。
	LimitBytes int64 `json:"limit_bytes"`
	// MemoryEntries 内存中保留的条数（环形缓冲容量）。
	MemoryEntries int `json:"memory_entries"`
	// ClearedAt 最近一次手动清空的时刻；nil = 从未清空。
	//
	// 用指针而不是 time.Time，是为了让"从未清空"在 JSON 里**缺席**：
	// time.Time 是 struct，omitempty 对 struct 一律不生效，零值会被序列化成
	// "0001-01-01T00:00:00Z"，而控制台用真值判断（`si.cleared_at ? ... : "从未清空"`）
	// 会照单全收 → 没清空过的实例显示「上次清空：08:00:00」。
	ClearedAt *time.Time `json:"cleared_at"`
	// LastError 最近一次落盘/轮转失败原因；非空意味着**上限可能已失效**，需人工介入。
	LastError string `json:"last_error,omitempty"`
}

// SizeInfo 读取当前占用与上限。加锁，可随时调用（控制台每次刷新都会调）。
func (l *Logger) SizeInfo() SizeInfo {
	if l == nil {
		return SizeInfo{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	si := SizeInfo{
		MaxBytes:      l.maxBytes,
		MaxBackups:    l.maxBk,
		MemoryEntries: l.size,
		LastError:     l.lastErr,
	}
	// 从未清空时保持 nil（JSON 里是 null），控制台才能区分"从未清空"与"某时刻清空过"。
	if !l.clearedAt.IsZero() {
		t := l.clearedAt
		si.ClearedAt = &t
	}

	if l.file == "" {
		return si
	}
	si.FileBytes = l.fileSize
	if fi, err := os.Stat(l.file); err == nil {
		si.FileBytes = fi.Size()
	}
	backupCount := l.maxBk
	if l.pruneBackups > backupCount {
		backupCount = l.pruneBackups
	}
	for i := 1; i <= backupCount; i++ {
		if fi, err := os.Stat(backupPath(l.file, i)); err == nil {
			si.BackupBytes += fi.Size()
		}
	}
	si.TotalBytes = si.FileBytes + si.BackupBytes
	if l.maxBytes > 0 {
		si.LimitBytes = l.maxBytes * int64(1+l.maxBk)
	}
	return si
}

// countLocked 已保留条数。调用方必须已持 l.mu。
func (l *Logger) countLocked() int {
	if l.filled {
		return l.size
	}
	return l.next
}

// Record 记录一条调用。环形缓冲始终写入；落盘仅在 File 非空时进行。
// 本条已带 Seq（由调用方从 NextSeq 取）时沿用，否则在此分配。
func (l *Logger) Record(e Entry) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.enabled {
		l.mu.Unlock()
		return
	}
	l.total++
	if e.Seq == 0 {
		e.Seq = l.total
	}
	l.ring[l.next] = e
	l.next = (l.next + 1) % l.size
	if l.next == 0 {
		l.filled = true
	}
	if l.file != "" {
		l.appendLocked(e)
	}
	l.mu.Unlock()
}

// NextSeq 取一个进程内单调递增的序号（与 stdout 表格日志的 #NNN 各自独立计数）。
func (l *Logger) NextSeq() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total + 1
}

// appendLocked 追加一行 JSONL，必要时先轮转。调用方必须已持 l.mu。
//
// 写失败不 panic 也不静默：记进 lastErr 供控制台显示（磁盘满是运维盲区，
// 而"日志没在写"和"日志在写但被静默丢弃"必须能区分）。
func (l *Logger) appendLocked(e Entry) {
	if l.rescanSize {
		l.recalibrateSizeLocked()
		l.rescanSize = false
	}
	for l.pruneBackups > l.maxBk {
		if err := os.Remove(backupPath(l.file, l.pruneBackups)); err != nil && !os.IsNotExist(err) {
			l.lastErr = "清理旧备份失败: " + err.Error()
			return
		}
		l.pruneBackups--
	}
	raw, err := json.Marshal(e)
	if err != nil {
		l.lastErr = "序列化失败: " + err.Error()
		return
	}
	// 单条过大：丢掉 usage 原样副本重写（那是最可能膨胀的字段，
	// 也是唯一"可以不要"的字段——其余都是标量）。防止单条撑爆整个文件、
	// 让轮转陷入"每写一条就轮转"的死循环。
	if len(raw) > maxEntryBytes {
		e.Usage = nil
		if raw2, err2 := json.Marshal(e); err2 == nil {
			raw = raw2
		}
	}
	raw = append(raw, '\n')
	if l.maxBytes > 0 && l.fileSize+int64(len(raw)) > l.maxBytes {
		l.rotateLocked()
	}
	f, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.lastErr = "打开日志失败: " + err.Error()
		return
	}
	n, werr := f.Write(raw)
	cerr := f.Close()
	if werr != nil {
		l.lastErr = "写入日志失败: " + werr.Error()
	} else if cerr != nil {
		l.lastErr = "关闭日志失败: " + cerr.Error()
	} else {
		l.lastErr = "" // 成功即清空，避免陈年错误常驻控制台
	}
	l.fileSize += int64(n)
}

// rotateLocked 轮转当前日志文件。调用方必须已持 l.mu。
//
// 保留 maxBk 份备份：先把最老的删掉，再把 .i 逐个后移为 .i+1（从大往小，
// 避免覆盖），最后当前文件 → .1。
//
// 失败处理是关键：改名/删除失败时**用 stat 重新校准 fileSize，绝不重置为 0**。
// 若在这里乐观地清零，而文件其实还在（改名失败），后续判断就永远认为"文件很小"，
// 上限静默失效、文件继续无限增长——正是本函数要防的那种爆炸。
func (l *Logger) rotateLocked() {
	// 世代号自增放在最前面，且不问成败：本次调用**意图**就是让既有累计口径作废
	// （搬到备份 / 直接丢弃），而成功路径的语义没有变化。放在失败分支之后会漏掉
	// "改名失败但仍按失败路径 recalibrate" 的情况——那条路径也可能已经删掉了最老备份。
	l.gen++
	if l.maxBk <= 0 {
		// 不保留备份：直接丢弃当前文件（磁盘占用恒为 1 个文件上限）。
		if err := os.Remove(l.file); err != nil && !os.IsNotExist(err) {
			l.lastErr = "轮转删除失败: " + err.Error()
			l.recalibrateSizeLocked()
			return
		}
		l.fileSize = 0
		return
	}
	// 删最老的一份（不存在则忽略）。
	_ = os.Remove(backupPath(l.file, l.maxBk))
	// .N-1 → .N …… .1 → .2（从大到小，避免后移时覆盖还没搬走的那份）。
	for i := l.maxBk - 1; i >= 1; i-- {
		_ = os.Rename(backupPath(l.file, i), backupPath(l.file, i+1))
	}
	if err := os.Rename(l.file, backupPath(l.file, 1)); err != nil {
		l.lastErr = "轮转改名失败: " + err.Error()
		l.recalibrateSizeLocked()
		return
	}
	l.fileSize = 0
}

// recalibrateSizeLocked 用磁盘真实大小重置 fileSize（轮转失败后的兜底）。
// 调用方必须已持 l.mu。
func (l *Logger) recalibrateSizeLocked() {
	if fi, err := os.Stat(l.file); err == nil {
		l.fileSize = fi.Size()
		return
	}
	// 文件已不存在（被外部删除）：置 0 让下次写入重建。
	l.fileSize = 0
}

// backupPath 第 n 份备份的路径（n 从 1 开始）。
func backupPath(file string, n int) string {
	return file + "." + strconv.Itoa(n)
}

// Clear 清空使用日志：内存环形缓冲、累计计数、落盘文件及其全部备份。
// 返回删除的总字节数（供控制台提示"释放了多少"）。
//
// 为何连内存一起清：控制台读的是环形缓冲，只删文件的话页面仍显示已"删除"的历史，
// 用户会以为没生效。手动清空是显式操作，语义上就该是"现在归零"。
//
// 备份扫描范围（clearScanBackups=64）比 maxBackups 宽：
// 清掉曾经配过更大 max_backups 时遗留的旧文件，避免它们永远占着磁盘。
func (l *Logger) Clear() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var removed int64
	if l.file != "" {
		if fi, err := os.Stat(l.file); err == nil {
			removed += fi.Size()
		}
		if err := os.Remove(l.file); err != nil && !os.IsNotExist(err) {
			l.lastErr = "清除日志失败: " + err.Error()
		} else {
			l.lastErr = ""
		}
		for i := 1; i <= clearScanBackups; i++ {
			bp := backupPath(l.file, i)
			if fi, err := os.Stat(bp); err == nil {
				removed += fi.Size()
			}
			if err := os.Remove(bp); err != nil && !os.IsNotExist(err) {
				l.lastErr = "清除备份失败: " + err.Error()
			}
		}
		l.fileSize = 0
	}
	// 内存态归零：清空引用避免残留数据被后续读取。
	for i := range l.ring {
		l.ring[i] = Entry{}
	}
	l.next, l.filled, l.total = 0, false, 0
	l.historyCache = nil
	l.clearedAt = time.Now()
	// 世代号自增：累计统计缓存必须立刻作废，否则清空后控制台仍显示旧总量。
	// 清空是新语义（什么都没了），与轮转（总量不减、只是老数据被删）共用一个计数器，
	// 因为两者的共同点正是"缓存里的旧数字不再对应当前磁盘内容"。
	l.gen++
	return removed
}

// Recent 返回最近 limit 条记录（按时间正序：最早 → 最新），limit<=0 视为全部保留条数。
func (l *Logger) Recent(limit int) []Entry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recentLocked(limit)
}

func (l *Logger) recentLocked(limit int) []Entry {
	n := l.countLocked()
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Entry, 0, limit)
	// 环形缓冲里最新一条位于 next-1；往前取 limit 条后正序输出。
	start := l.next - limit
	if start < 0 {
		start += l.size
	}
	for i := 0; i < limit; i++ {
		out = append(out, l.ring[(start+i)%l.size])
	}
	return out
}

// String 便于日志/调试输出（"占用 1.2MB / 上限 64MB"）。
func (si SizeInfo) String() string {
	return fmt.Sprintf("占用 %.1fMB / 上限 %.1fMB（主 %.1fMB + 备份 %.1fMB，保留 %d 份）",
		mb(si.TotalBytes), mb(si.LimitBytes), mb(si.FileBytes), mb(si.BackupBytes), si.MaxBackups)
}

func mb(n int64) float64 { return float64(n) / (1 << 20) }

// CreditOf 从上游 usage 提取本次消耗的积分（usage.credit）。
//
// 实测（workbuddy ai 线 2026-09-17 三笔真实调用）：上游每次都在 usage 里返回
// 精确的消耗值，如 "credit":10.85 / 11 / 10.82。这是**上游自己算的账**，
// 比"调用前后余额差值"可靠得多——实测差值恒为 0（余额接口不即时反映消耗），
// 而 credit 一直是准的。
//
// 返回 (值, 是否可用)。缺失/类型不符/负值一律视为不可用（调用方回退差值法）。
func CreditOf(u map[string]any) (float64, bool) {
	if len(u) == 0 {
		return 0, false
	}
	switch v := u["credit"].(type) {
	case float64:
		// 负值无意义（上游不会倒给分），视为脏数据不可用。
		if v < 0 {
			return 0, false
		}
		return v, true
	case json.Number:
		if f, err := v.Float64(); err == nil && f >= 0 {
			return f, true
		}
	}
	return 0, false
}
