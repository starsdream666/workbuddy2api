package usagelog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// int64p 便于构造指针字段（Entry 用指针表达"未知"）。
func int64p(v int64) *int64 { return &v }

// float64p 同理（CreditsUsed 是小数：上游 credit 精确到 0.01）。
func float64p(v float64) *float64 { return &v }

// TestDisabledLoggerIsNoop 关闭时 Record 不占内存也不落盘。
func TestDisabledLoggerIsNoop(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: false, File: fp})
	if l.Enabled() {
		t.Fatal("Enabled() must be false")
	}
	l.Record(Entry{UID: "u1"})
	if got := l.Recent(0); len(got) != 0 {
		t.Errorf("disabled logger kept %d entries", len(got))
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Errorf("disabled logger must not create the file (err=%v)", err)
	}
}

// TestNilLoggerSafe nil 接收者不 panic（handler 不注入日志器时的默认路径）。
func TestNilLoggerSafe(t *testing.T) {
	var l *Logger
	if l.Enabled() {
		t.Error("nil logger must report disabled")
	}
	l.Record(Entry{UID: "u1"})
	if got := l.Recent(10); got != nil {
		t.Errorf("Recent=%v want nil", got)
	}
	if total, kept, file, enabled := l.Stats(); total != 0 || kept != 0 || file != "" || enabled {
		t.Errorf("Stats=%d/%d/%q/%v want zeros", total, kept, file, enabled)
	}
}

// TestRecentReturnsOldestToNewest 环形缓冲按时间正序返回，且条数受 limit 限制。
func TestRecentReturnsOldestToNewest(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 4})
	for i := 1; i <= 3; i++ {
		l.Record(Entry{UID: "u" + string(rune('0'+i))})
	}
	got := l.Recent(0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for i, want := range []string{"u1", "u2", "u3"} {
		if got[i].UID != want {
			t.Errorf("pos %d: uid=%q want %q", i, got[i].UID, want)
		}
	}
	if r := l.Recent(2); len(r) != 2 || r[1].UID != "u3" {
		t.Errorf("limit=2 got %v want last two ending u3", r)
	}
}

// TestRingWrapsAndKeepsNewest 环形缓冲写满后覆盖最旧，Recent 仍给最新 N 条正序。
func TestRingWrapsAndKeepsNewest(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 3})
	for i := 1; i <= 7; i++ {
		l.Record(Entry{UID: string(rune('a' + i - 1))})
	}
	got := l.Recent(0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (capacity)", len(got))
	}
	// 7 条容量 3：应保留第 5/6/7 条 = e/f/g，正序。
	for i, want := range []string{"e", "f", "g"} {
		if got[i].UID != want {
			t.Errorf("pos %d: uid=%q want %q", i, got[i].UID, want)
		}
	}
	// 累计计数不受容量影响。
	if total, kept, _, _ := l.Stats(); total != 7 || kept != 3 {
		t.Errorf("Stats total=%d kept=%d want 7/3", total, kept)
	}
}

// TestSeqAssignedMonotonically 未显式给 Seq 时按累计编号；显式给了就沿用。
func TestSeqAssignedMonotonically(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 8})
	l.Record(Entry{UID: "a"})
	l.Record(Entry{UID: "b"})
	l.Record(Entry{Seq: 99, UID: "c"}) // 显式序号应被保留
	got := l.Recent(0)
	if got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 99 {
		t.Errorf("seq=%d/%d/%d want 1/2/99", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	if n := l.NextSeq(); n != 4 {
		t.Errorf("NextSeq=%d want 4", n)
	}
}

// TestAppendWritesJSONL 落盘为 JSONL：一行一条、可解析、积分指针字段按 null 表达未知。
func TestAppendWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})

	// 第一条：积分未知（指针全 nil）→ JSON 里应为 null。
	l.Record(Entry{UID: "unknown", CreditsKnown: false})
	// 第二条：积分已知。
	l.Record(Entry{
		UID:          "known",
		CreditsBefore: int64p(350),
		CreditsAfter:  int64p(347),
		CreditsUsed:   float64p(3),
		CreditsKnown:  true,
		PromptTokens:  10,
		CachedTokens:  8,
	})

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := splitLines(string(raw))
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2:\n%s", len(lines), raw)
	}
	var first, second Entry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line1 not json: %v", err)
	}
	if first.CreditsUsed != nil || first.CreditsKnown {
		t.Errorf("unknown credits must stay null: used=%v known=%v", first.CreditsUsed, first.CreditsKnown)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line2 not json: %v", err)
	}
	if second.CreditsUsed == nil || *second.CreditsUsed != 3 {
		t.Errorf("credits_used=%v want 3", second.CreditsUsed)
	}
	if second.CachedTokens != 8 {
		t.Errorf("cached_tokens=%d want 8", second.CachedTokens)
	}
	if second.BalanceError != "" {
		t.Errorf("balance_error=%q want empty", second.BalanceError)
	}
}

// splitLines 按行切分并丢掉结尾空行。
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != '\r' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// TestRotateOnSizeLimit 超过大小上限时轮转为 <file>.1（单份备份），
// 且 fileSize 归零后继续写入新文件（不会因为旧尺寸残留而每写一条都轮转）。
func TestRotateOnSizeLimit(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	// 注入很小的字节上限，让轮转可被真实触发（MaxSizeMB 最小非零值是 1MB，太重）。
	l := New(Config{Enabled: true, File: fp, MaxBytes: 200})
	// 写到超过上限：必然发生轮转。
	for i := 0; i < 20; i++ {
		l.Record(Entry{UID: "u-with-some-padding", Model: "glm-5.2-padding"})
	}
	if _, err := os.Stat(fp + ".1"); err != nil {
		t.Fatalf("rotation should have produced a backup file: %v", err)
	}
	// 轮转后主文件必须存在且非空（新数据继续写进主文件，而不是全被搬走）。
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("main log should exist after rotation: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("main log should keep receiving entries after rotation")
	}
	// 单份备份语义：主文件 + .1，不会出现 .2。
	if _, err := os.Stat(fp + ".2"); !os.IsNotExist(err) {
		t.Errorf("rotation must keep exactly one backup (.1): %v", err)
	}
}

// TestRestartPicksUpExistingFileSize 重启（对新路径重新 New）时 fileSize 从磁盘
// 现有大小续算，而不是从 0 开始——否则已接近上限的文件在重启后要很久才轮转。
func TestRestartPicksUpExistingFileSize(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 100000})
	l.Record(Entry{UID: "u1", Model: "m"})

	l2 := New(Config{Enabled: true, File: fp, MaxBytes: 100000})
	l2.mu.Lock()
	got := l2.fileSize
	l2.mu.Unlock()
	if got <= 0 {
		t.Errorf("fileSize after restart=%d want >0 (should resume from disk size)", got)
	}
}

// TestRestartReloadsRecentFromFile 重启后 Recent() 必须能读到磁盘上的历史。
//
// 这是用户可见的问题：内存环形缓冲是进程态、重启即丢，而控制台只读它。
// 不回填就等于"日志文件明明还在、页面却一片空白"，让人以为日志丢了。
func TestRestartReloadsRecentFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MemorySize: 100})
	for i := 1; i <= 5; i++ {
		l.Record(Entry{UID: "u1", Model: "m" + strconv.Itoa(i), CreditsUsed: float64p(float64(i))})
	}

	// 模拟容器重启：同一路径新建 Logger（旧实例的内存全丢）。
	l2 := New(Config{Enabled: true, File: fp, MemorySize: 100})
	got := l2.Recent(0)
	if len(got) != 5 {
		t.Fatalf("after restart Recent=%d 条，want 5（应从文件回填历史）", len(got))
	}
	// 正序：最旧的在前，最新的在最后。
	if got[0].Model != "m1" || got[4].Model != "m5" {
		t.Errorf("order wrong: first=%q last=%q want m1..m5", got[0].Model, got[4].Model)
	}
	// 积分等字段也要完整还原（不能只剩 UID）。
	if got[4].CreditsUsed == nil || *got[4].CreditsUsed != 5 {
		t.Errorf("credits_used=%v want 5 (full entry must survive reload)", got[4].CreditsUsed)
	}
}

// TestRestartReloadHonorsMemorySize 回填量以环形缓冲容量为上限（不把整个文件读进来）。
func TestRestartReloadHonorsMemorySize(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MemorySize: 100})
	for i := 1; i <= 20; i++ {
		l.Record(Entry{UID: "u1", Model: "m" + strconv.Itoa(i)})
	}

	l2 := New(Config{Enabled: true, File: fp, MemorySize: 3})
	got := l2.Recent(0)
	if len(got) != 3 {
		t.Fatalf("Recent=%d want 3 (容量上限)", len(got))
	}
	// 保留的必须是**最新** 3 条（m18/m19/m20），不是最旧的。
	if got[0].Model != "m18" || got[2].Model != "m20" {
		t.Errorf("kept %q..%q want m18..m20 (newest)", got[0].Model, got[2].Model)
	}
}

// TestRestartReloadSkipsCorruptLines 坏行（手工改坏 / 写入被截断）不该毁掉整段历史。
func TestRestartReloadSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	valid := func(name string) string {
		b, _ := json.Marshal(Entry{UID: "u1", Model: name})
		return string(b)
	}
	content := valid("m1") + "\n" +
		"{这不是合法 JSON\n" + // 坏行
		valid("m3") + "\n" +
		`{"uid":"u9","model":"m4"` + "\n" // 被截断的半行
	os.WriteFile(fp, []byte(content), 0o600)

	l := New(Config{Enabled: true, File: fp, MemorySize: 10})
	got := l.Recent(0)
	if len(got) != 2 {
		t.Fatalf("Recent=%d want 2（两条合法记录，坏行跳过）", len(got))
	}
	if got[0].Model != "m1" || got[1].Model != "m3" {
		t.Errorf("got %q,%q want m1,m3", got[0].Model, got[1].Model)
	}
}

// TestRestartReloadContinuesSeq 回填后新记录的序号要继续递增，不能与历史撞号。
//
// 撞号的后果：控制台按 seq 排序/去重时会错乱，且无法区分"重启前"与"重启后"。
func TestRestartReloadContinuesSeq(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MemorySize: 50})
	for i := 0; i < 4; i++ {
		l.Record(Entry{UID: "u1"})
	}

	l2 := New(Config{Enabled: true, File: fp, MemorySize: 50})
	// 回填了 4 条 → 下一个序号应当是 5。
	if seq := l2.NextSeq(); seq != 5 {
		t.Errorf("NextSeq after reload=%d want 5 (must continue past history)", seq)
	}
}

// TestRestartReloadNoFileIsSafe 文件不存在（首次启动）时回填必须静默跳过。
func TestRestartReloadNoFileIsSafe(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "not-there.jsonl")
	l := New(Config{Enabled: true, File: fp, MemorySize: 10})
	if got := l.Recent(0); len(got) != 0 {
		t.Errorf("Recent=%d want 0 for missing file", len(got))
	}
	// 之后仍能正常记录。
	l.Record(Entry{UID: "u1"})
	if got := l.Recent(0); len(got) != 1 {
		t.Errorf("Recent=%d want 1 after first record", len(got))
	}
}

// TestNewCreatesParentDir 落盘目录不存在时应被创建（控制台/文件都能用）。
func TestNewCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "nested", "deep", "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	if !l.Enabled() {
		t.Fatal("logger should be enabled")
	}
	l.Record(Entry{UID: "u1"})
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("nested dir/file should be created: %v", err)
	}
}

// TestMaxBackupsBoundsDiskUsage 磁盘占用受 max_backups 硬约束：
// 多份备份时写大量数据，主文件 + 全部备份的合计不得超过 limit_bytes。
// 这是"避免日志爆炸"的核心保证。
func TestMaxBackupsBoundsDiskUsage(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 2000, MaxBackups: 3, BackupsSet: true})

	// 写远超上限的数据（50KB），触发多轮轮转。
	pad := strings.Repeat("x", 200)
	for i := 0; i < 300; i++ {
		l.Record(Entry{UID: "u1", Model: pad})
	}

	si := l.SizeInfo()
	if si.LimitBytes != 2000*4 {
		t.Fatalf("limit_bytes=%d want 8000 (=maxBytes×(1+backups))", si.LimitBytes)
	}
	total := dirUsage(t, dir)
	if total > si.LimitBytes {
		t.Errorf("disk usage %d exceeds hard limit %d — logs would explode", total, si.LimitBytes)
	}
	// 备份必须真的产生了（否则轮转没生效，恒单文件）。
	if _, err := os.Stat(fp + ".1"); err != nil {
		t.Errorf("expected at least one backup: %v", err)
	}
	// 且不得超过配置的份数。
	if _, err := os.Stat(fp + ".4"); !os.IsNotExist(err) {
		t.Errorf("must not keep more than max_backups=3 backups: %v", err)
	}
}

// TestMaxBackupsZeroKeepsSingleFile max_backups=0 → 不留备份，占用恒为 1 个文件上限。
func TestMaxBackupsZeroKeepsSingleFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 1000, MaxBackups: 0, BackupsSet: true})
	pad := strings.Repeat("y", 200)
	for i := 0; i < 100; i++ {
		l.Record(Entry{UID: "u1", Model: pad})
	}
	if _, err := os.Stat(fp + ".1"); !os.IsNotExist(err) {
		t.Errorf("max_backups=0 must not create backups: %v", err)
	}
	si := l.SizeInfo()
	if si.BackupBytes != 0 {
		t.Errorf("backup_bytes=%d want 0", si.BackupBytes)
	}
	if total := dirUsage(t, dir); total > si.LimitBytes {
		t.Errorf("disk usage %d exceeds limit %d with max_backups=0", total, si.LimitBytes)
	}
}

// dirUsage 目录内所有文件字节合计（含备份）。
func dirUsage(t *testing.T, dir string) int64 {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var total int64
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// TestClearRemovesEverything Clear 必须抹掉内存 + 主文件 + 全部备份，
// 并返回释放的字节数（控制台据此提示）。清空后 Recent 必须为空。
func TestClearRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 800, MaxBackups: 2, BackupsSet: true})
	pad := strings.Repeat("z", 150)
	for i := 0; i < 60; i++ {
		l.Record(Entry{UID: "u1", Model: pad})
	}
	if _, err := os.Stat(fp + ".1"); err != nil {
		t.Fatalf("fixture should have produced a backup: %v", err)
	}
	before := dirUsage(t, dir)
	if before == 0 {
		t.Fatal("fixture should have written something")
	}

	removed := l.Clear()

	if removed <= 0 {
		t.Errorf("removed=%d want >0 (bytes freed)", removed)
	}
	if after := dirUsage(t, dir); after != 0 {
		t.Errorf("dir still holds %d bytes after Clear", after)
	}
	if got := l.Recent(0); len(got) != 0 {
		t.Errorf("memory ring must be cleared too, got %d entries", len(got))
	}
	if total, kept, _, _ := l.Stats(); total != 0 || kept != 0 {
		t.Errorf("counters must reset: total=%d kept=%d", total, kept)
	}
	si := l.SizeInfo()
	if si.FileBytes != 0 || si.TotalBytes != 0 {
		t.Errorf("size must read zero after Clear: %+v", si)
	}
	if si.ClearedAt.IsZero() {
		t.Error("cleared_at should be stamped")
	}
	// 清空后仍能继续记录（不是把自己弄坏了）。
	l.Record(Entry{UID: "u2", Model: "m"})
	if got := l.Recent(0); len(got) != 1 || got[0].UID != "u2" {
		t.Errorf("logger must keep working after Clear: %+v", got)
	}
}

// TestClearScansBeyondConfiguredBackups Clear 要能清掉"曾经配过更大 max_backups"
// 遗留下来的旧备份，否则它们会永远占着磁盘（Clear 的意义就是彻底回收）。
func TestClearScansBeyondConfiguredBackups(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	// 手工造出 .1 … .5（模拟历史上 max_backups 曾设得更大）。
	for i := 1; i <= 5; i++ {
		if err := os.WriteFile(fp+"."+strconv.Itoa(i), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := New(Config{Enabled: true, File: fp, MaxBackups: 1, BackupsSet: true})
	l.Clear()
	for i := 1; i <= 5; i++ {
		p := fp + "." + strconv.Itoa(i)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("Clear must remove stale backup %s: %v", p, err)
		}
	}
}

// TestOversizedEntryDropsUsageCopy 单条记录过大时丢弃 usage 原样副本重写：
// 防止单条撑爆整个文件、让轮转陷入"每写一条就轮转"的死循环。
func TestOversizedEntryDropsUsageCopy(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 1 << 20, MaxBackups: 1, BackupsSet: true})

	// 造一个远超 maxEntryBytes(64KB) 的 usage 副本。
	huge := map[string]any{}
	for i := 0; i < 4000; i++ {
		huge["k"+strconv.Itoa(i)] = strings.Repeat("v", 50)
	}
	l.Record(Entry{UID: "u1", Model: "m", Usage: huge})

	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("log should exist: %v", err)
	}
	if fi.Size() > maxEntryBytes {
		t.Errorf("entry size %d should have been capped near %d", fi.Size(), maxEntryBytes)
	}
	// 标量字段必须保留（丢的是可选的 usage 副本，不是整条记录）。
	raw, _ := os.ReadFile(fp)
	if !strings.Contains(string(raw), `"uid":"u1"`) {
		t.Errorf("scalar fields must survive the usage-drop rewrite: %s", raw)
	}
	if strings.Contains(string(raw), `"usage"`) {
		t.Error("oversized usage copy should have been dropped")
	}
}

// TestSizeInfoMemoryOnly 只留内存（File 为空）时占用恒为 0，上限也报 0。
func TestSizeInfoMemoryOnly(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 10}) // 无 File
	l.Record(Entry{UID: "u1"})
	si := l.SizeInfo()
	if si.TotalBytes != 0 || si.LimitBytes != 0 {
		t.Errorf("memory-only logger should report zero disk usage/limit: %+v", si)
	}
	if si.MemoryEntries != 10 {
		t.Errorf("memory_entries=%d want 10", si.MemoryEntries)
	}
	if removed := l.Clear(); removed != 0 {
		t.Errorf("Clear on memory-only logger removed=%d want 0", removed)
	}
	if got := l.Recent(0); len(got) != 0 {
		t.Errorf("Clear must still empty the memory ring: %+v", got)
	}
}

// TestBackupsSetZeroValueSemantics 锁定 Config 的零值契约（防回归）：
// Go 零值 MaxBackups=0 不得被解释成"不留备份"——否则任何零值构造 Config 的地方
// （包括漏配字段的生产部署）都会静默把磁盘占用上限砍半。
// 只有显式 BackupsSet=true 且 MaxBackups=0 才是"不留备份"。
func TestBackupsSetZeroValueSemantics(t *testing.T) {
	dir := t.TempDir()
	pad := strings.Repeat("q", 200)

	// 零值（未显式设置）→ 用默认 1 份备份。
	fpZero := filepath.Join(dir, "zero.jsonl")
	lz := New(Config{Enabled: true, File: fpZero, MaxBytes: 1000})
	for i := 0; i < 40; i++ {
		lz.Record(Entry{UID: "u1", Model: pad})
	}
	if _, err := os.Stat(fpZero + ".1"); err != nil {
		t.Errorf("zero-value MaxBackups must fall back to default 1 backup: %v", err)
	}
	if si := lz.SizeInfo(); si.MaxBackups != defaultMaxBackups {
		t.Errorf("MaxBackups=%d want default %d when BackupsSet=false", si.MaxBackups, defaultMaxBackups)
	}

	// 显式 BackupsSet=true + 0 → 不留备份。
	fpExplicit := filepath.Join(dir, "explicit.jsonl")
	le := New(Config{Enabled: true, File: fpExplicit, MaxBytes: 1000, MaxBackups: 0, BackupsSet: true})
	for i := 0; i < 40; i++ {
		le.Record(Entry{UID: "u1", Model: pad})
	}
	if _, err := os.Stat(fpExplicit + ".1"); !os.IsNotExist(err) {
		t.Errorf("explicit MaxBackups=0 must not create backups: %v", err)
	}
	if si := le.SizeInfo(); si.MaxBackups != 0 {
		t.Errorf("explicit MaxBackups=0 should be honoured, got %d", si.MaxBackups)
	}
}