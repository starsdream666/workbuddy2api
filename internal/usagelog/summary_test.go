package usagelog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeEntries 用真实落盘路径写若干条记录（不用内存缓冲，因为累计统计只认文件）。
func writeEntries(t *testing.T, l *Logger, n int, mutate func(i int) Entry) {
	t.Helper()
	for i := 0; i < n; i++ {
		e := Entry{UID: "u1", Realm: "ai", Model: "glm-5.2", Status: 200, Time: time.Unix(int64(1700000000+i), 0)}
		if mutate != nil {
			e = mutate(i)
		}
		l.Record(e)
	}
}

// countFileRecords 数一个 JSONL 文件里**真正可解析**的记录条数（跳过坏行）。
func countFileRecords(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, ln := range splitLines(string(raw)) {
		var e Entry
		if json.Unmarshal([]byte(ln), &e) == nil {
			n++
		}
	}
	return n
}

// TestSummarySumsTotals 累计口径的基线：token 三项、缓存、额度、成功/失败、时间范围。
func TestSummarySumsTotals(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MemorySize: 4})
	// 容量 4 的环形缓冲只留 4 条，但累计统计必须覆盖**全部 6 条**——
	// 这正是"累计不读内存缓冲"的核心保证（否则数字会随窗口滚动倒退）。
	used := float64(3)
	writeEntries(t, l, 6, func(i int) Entry {
		e := Entry{UID: "u1", Realm: "ai", Model: "glm-5.2", Status: 200, Time: time.Unix(int64(1700000000+i), 0),
			PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 60}
		if i == 5 {
			e.Status = 500 // 一条失败
		}
		e.CreditsKnown, e.CreditsUsed = true, &used
		return e
	})

	s := l.Summary()
	if s.Requests != 6 {
		t.Errorf("requests=%d want 6（累计必须覆盖整个文件，而不是内存环形缓冲的 4 条）", s.Requests)
	}
	if s.Success != 5 || s.Failed != 1 || s.Success+s.Failed != s.Requests {
		t.Errorf("success=%d failed=%d want 5/1 且和等于 requests", s.Success, s.Failed)
	}
	if s.PromptTokens != 600 || s.CompletionTokens != 120 || s.TotalTokens != 720 || s.CachedTokens != 360 {
		t.Errorf("tokens=%d/%d/%d cached=%d want 600/120/720/360",
			s.PromptTokens, s.CompletionTokens, s.TotalTokens, s.CachedTokens)
	}
	if s.CreditsUsed != 18 {
		t.Errorf("credits_used=%v want 18 (6×3)", s.CreditsUsed)
	}
	if s.CreditsUnknown != 0 {
		t.Errorf("credits_unknown=%d want 0", s.CreditsUnknown)
	}
	// 时间范围覆盖首末两条。
	if got := s.FirstTime.Unix(); got != 1700000000 {
		t.Errorf("first_time=%d want 1700000000", got)
	}
	if got := s.LastTime.Unix(); got != 1700000005 {
		t.Errorf("last_time=%d want 1700000005", got)
	}
}

// TestSummaryCountsUnknownCreditsSeparately 未观测到消耗的调用计入 credits_unknown，
// 绝不当成 0 混进 credits_used（否则额度统计看起来完整、实际少算）。
func TestSummaryCountsUnknownCreditsSeparately(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	used := float64(2.5)
	l.Record(Entry{UID: "u1", Status: 200, CreditsKnown: true, CreditsUsed: &used})
	l.Record(Entry{UID: "u1", Status: 200}) // 积分未知（上游没给 credit、差值也不可用）

	s := l.Summary()
	if s.CreditsUsed != 2.5 {
		t.Errorf("credits_used=%v want 2.5（未知的那条不得被当成 0 参与求和）", s.CreditsUsed)
	}
	if s.CreditsUnknown != 1 {
		t.Errorf("credits_unknown=%d want 1（必须显式暴露「未记录消耗」的条数）", s.CreditsUnknown)
	}
}

// TestSummaryGroupsByRealmUIDModel 三个维度的分组、完整 uid 键、排序与"分组求和 == 合计"不变量。
func TestSummaryGroupsByRealmUIDModel(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	// u1(ai) 2 条、u2(cn) 1 条、u3 无 realm 1 条 → 按 realm 分组应有 ai/cn/未标注。
	l.Record(Entry{UID: "11111111-aaaa-bbbb-cccc-000000000001", Realm: "ai", Model: "glm-5.2", Status: 200, PromptTokens: 10})
	l.Record(Entry{UID: "11111111-aaaa-bbbb-cccc-000000000001", Realm: "ai", Model: "glm-5.2", Status: 200, PromptTokens: 10})
	l.Record(Entry{UID: "22222222-dddd-eeee-ffff-000000000002", Realm: "cn", Model: "claude-x", Status: 200, PromptTokens: 5})
	l.Record(Entry{UID: "u3", Model: "", Status: 200})

	s := l.Summary()
	if len(s.ByRealm) != 3 {
		t.Fatalf("by_realm=%d 组 want 3: %+v", len(s.ByRealm), s.ByRealm)
	}
	// 排序：requests 倒序 → ai(2) 在首位。
	if s.ByRealm[0].Key != "ai" || s.ByRealm[0].Totals.Requests != 2 {
		t.Errorf("first realm group=%q requests=%d want ai/2", s.ByRealm[0].Key, s.ByRealm[0].Totals.Requests)
	}
	// 空 realm / 空 model 统一落到"未标注"，不留没有名字的分组。
	found := false
	for _, g := range s.ByRealm {
		if g.Key == "未标注" {
			found = true
		}
	}
	if !found {
		t.Errorf("empty realm must be grouped under 未标注: %+v", s.ByRealm)
	}
	// 账号维度的键是**完整 uid**（截断是展示层的事：前 8 位可能撞车）。
	if len(s.ByUID) != 3 {
		t.Fatalf("by_uid=%d 组 want 3: %+v", len(s.ByUID), s.ByUID)
	}
	for _, g := range s.ByUID {
		if g.Key == "u1" {
			t.Errorf("uid key must not be truncated: %+v", s.ByUID)
		}
	}
	// 分组求和必须等于合计（可加性是这组字段的设计前提，破例即说明加法漏了字段）。
	for _, dim := range [][]Group{s.ByRealm, s.ByUID, s.ByModel} {
		var acc Totals
		for _, g := range dim {
			acc.AddTotals(g.Totals)
		}
		if acc.Requests != s.Requests || acc.PromptTokens != s.PromptTokens ||
			acc.TotalTokens != s.TotalTokens || acc.CreditsUnknown != s.CreditsUnknown {
			t.Errorf("group sum %+v != overall %+v", acc, s.Totals)
		}
	}
}

// TestSummaryIncludesBackups 累计口径必须覆盖**主文件 + 备份**：
// 只统计主文件会漏掉轮转搬走的历史（页面上总量凭空变少）。
func TestSummaryIncludesBackups(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "usage.jsonl")
	l := New(Config{Enabled: true, File: fp, MaxBytes: 400, MaxBackups: 1, BackupsSet: true})
	writeEntries(t, l, 30, func(i int) Entry {
		return Entry{UID: "u1", Realm: "ai", Model: "glm-5.2-padding-to-force-rotation", Status: 200}
	})
	if _, err := os.Stat(fp + ".1"); err != nil {
		t.Fatalf("fixture should have produced a backup: %v", err)
	}

	want := countFileRecords(t, fp) + countFileRecords(t, fp+".1")
	if want == 0 {
		t.Fatal("fixture wrote nothing")
	}
	got := l.Summary().Requests
	if int(got) != want {
		t.Errorf("summary requests=%d want %d（主文件+备份里全部保留记录，只统计主文件会少算）", got, want)
	}
}

// TestSummarySkipsCorruptLines 坏行只跳过它自己，不能毁掉整份统计。
func TestSummarySkipsCorruptLines(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	valid := func(uid string, tokens int) string {
		b, _ := json.Marshal(Entry{UID: uid, Status: 200, PromptTokens: tokens})
		return string(b)
	}
	content := valid("u1", 10) + "\n" +
		"{这不是合法 JSON\n" +
		valid("u1", 10) + "\n" +
		`{"uid":"u9","status":` + "\n" // 截断的半行
	os.WriteFile(fp, []byte(content), 0o600)

	l := New(Config{Enabled: true, File: fp})
	s := l.Summary()
	if s.Requests != 2 {
		t.Errorf("requests=%d want 2（两条合法记录，坏行跳过）", s.Requests)
	}
	if s.PromptTokens != 20 {
		t.Errorf("prompt_tokens=%d want 20", s.PromptTokens)
	}
}

// TestSummaryCacheInvalidatedByRecord 新记录写入后统计必须更新（total 变化即失效）。
func TestSummaryCacheInvalidatedByRecord(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	used := float64(7)
	l.Record(Entry{UID: "u1", Status: 200, CreditsKnown: true, CreditsUsed: &used})

	first := l.Summary()
	if first.Requests != 1 || first.CreditsUsed != 7 {
		t.Fatalf("first summary=%+v want requests=1 credits=7", first)
	}
	// 第二次调用会命中缓存（total/gen 未变）→ 结果相同。
	if again := l.Summary(); again.Requests != 1 || again.CreditsUsed != 7 {
		t.Fatalf("cached summary=%+v want same as first", again)
	}
	// 再写一条：旧快照必须失效，否则页面会长期显示陈旧总量。
	l.Record(Entry{UID: "u1", Status: 200, CreditsKnown: true, CreditsUsed: &used})
	third := l.Summary()
	if third.Requests != 2 || third.CreditsUsed != 14 {
		t.Errorf("after new record summary=%+v want requests=2 credits=14", third)
	}
}

// TestSummaryZeroedAfterClear 清空后统计必须归零（gen 自增使缓存失效）。
func TestSummaryZeroedAfterClear(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	writeEntries(t, l, 3, nil)
	if l.Summary().Requests != 3 {
		t.Fatalf("fixture summary should be 3")
	}
	l.Clear()
	s := l.Summary()
	if s.Requests != 0 || s.TotalTokens != 0 || s.CreditsUsed != 0 {
		t.Errorf("summary after Clear=%+v want all zero", s)
	}
	// 数组必须是空数组而不是 null，前端才能直接遍历。
	if s.ByUID == nil || s.ByRealm == nil || s.ByModel == nil {
		t.Errorf("group slices must be empty (not nil) after Clear: %+v", s)
	}
}

// TestSummaryMemoryOnlyIsZero 只有内存（不落盘）时统计恒为零。
//
// 这是**有意**的：累计口径只能来自文件，若改为对内存环形缓冲求和，
// 数字会随窗口滚动而倒退（看着像权威账本，实际是会缩水的窗口和）。
func TestSummaryMemoryOnlyIsZero(t *testing.T) {
	l := New(Config{Enabled: true, MemorySize: 10}) // 无 File
	writeEntries(t, l, 3, nil)
	s := l.Summary()
	if s.Requests != 0 {
		t.Errorf("memory-only summary requests=%d want 0（宁可报不可用，也不给会缩水的假累计）", s.Requests)
	}
	if s.ByUID == nil {
		t.Errorf("group slices must be empty arrays, not nil: %+v", s)
	}
}

// TestSummaryDisabledAndNilSafe 关闭 / nil logger 都返回零值快照，不 panic。
func TestSummaryDisabledAndNilSafe(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	off := New(Config{Enabled: false, File: fp})
	off.Record(Entry{UID: "u1"})
	if s := off.Summary(); s.Requests != 0 {
		t.Errorf("disabled summary requests=%d want 0", s.Requests)
	}
	var nilLogger *Logger
	if s := nilLogger.Summary(); s.Requests != 0 || s.ByRealm == nil {
		t.Errorf("nil logger summary=%+v want zeros with empty arrays", s)
	}
}

// TestSummaryFoldsOverflowGroups 分组数超上限时折叠为"其他"，且**总额不丢**。
func TestSummaryFoldsOverflowGroups(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	total := summaryGroupLimit + 10
	used := float64(1)
	for i := 0; i < total; i++ {
		l.Record(Entry{
			UID:          fmt.Sprintf("uid-%03d", i),
			Realm:        "ai",
			Model:        "glm-5.2",
			Status:       200,
			PromptTokens: 10,
			CreditsKnown: true, CreditsUsed: &used,
		})
	}

	s := l.Summary()
	// 保留上限 + 1 个折叠桶。
	if len(s.ByUID) != summaryGroupLimit+1 {
		t.Fatalf("by_uid=%d 组 want %d（上限 + 其他）", len(s.ByUID), summaryGroupLimit+1)
	}
	last := s.ByUID[len(s.ByUID)-1]
	if last.Key != summaryOtherKey {
		t.Errorf("folded bucket must sit last and be named %q: %+v", summaryOtherKey, s.ByUID[len(s.ByUID)-1])
	}
	if last.Totals.Requests != 10 {
		t.Errorf("其他 bucket requests=%d want 10（被折叠的键数=10，各 1 条）", last.Totals.Requests)
	}
	if s.GroupOverflowKeys != 10 {
		t.Errorf("group_overflow_keys=%d want 10（前端据此提示部分维度已合并）", s.GroupOverflowKeys)
	}
	// 折叠不能丢账：总额仍等于全部记录之和。
	if s.Requests != int64(total) || s.PromptTokens != int64(total*10) || s.CreditsUsed != float64(total) {
		t.Errorf("summary after folding=%+v want requests=%d tokens=%d credits=%d",
			s, total, total*10, total)
	}
}

// TestSummaryJSONShape 锁定 JSON 契约（前端按这些键读取，改名即破坏控制台）。
func TestSummaryJSONShape(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	used := float64(1.25)
	// 带 Time：本测试要同时锁定 first_time/last_time 会被序列化成 RFC3339 字符串。
	l.Record(Entry{UID: "u1", Realm: "ai", Model: "m", Status: 200, PromptTokens: 3, CompletionTokens: 4,
		TotalTokens: 7, CachedTokens: 2, CreditsKnown: true, CreditsUsed: &used,
		DurationMS: 150, TTFBMS: 30, Time: time.Unix(1700000000, 0)})

	raw, err := json.Marshal(l.Summary())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 内嵌的 Totals 必须**铺平**在顶层（前端读 summary.requests 而不是 summary.totals.requests）。
	for _, k := range []string{"requests", "success", "failed", "prompt_tokens", "completion_tokens",
		"total_tokens", "cached_tokens", "credits_used", "credits_unknown", "credits_failed_unknown",
		"duration_sum_ms", "timed_count", "ttfb_sum_ms", "ttfb_count",
		"by_realm", "by_uid", "by_model", "group_overflow_keys", "scan_incomplete"} {
		if _, ok := m[k]; !ok {
			t.Errorf("summary JSON missing key %q: %s", k, raw)
		}
	}
	// 分组元素的形状：{key, totals:{...}}。
	groups, ok := m["by_uid"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("by_uid=%v want 1 group", m["by_uid"])
	}
	g, _ := groups[0].(map[string]any)
	if g["key"] != "u1" {
		t.Errorf("group key=%v want u1", g["key"])
	}
	if _, ok := g["totals"].(map[string]any); !ok {
		t.Errorf("group totals missing/wrong type: %v", g["totals"])
	}
	// 时间范围序列化为 RFC3339（前端 new Date() 直接可解析）。
	// 这条记录带 Time，故 first/last 都必须是可解析字符串，不能是 null。
	if s, _ := m["first_time"].(string); !strings.Contains(s, "T") {
		t.Errorf("first_time=%v want RFC3339 string", m["first_time"])
	}
	if s, _ := m["last_time"].(string); !strings.Contains(s, "T") {
		t.Errorf("last_time=%v want RFC3339 string", m["last_time"])
	}
}

// TestSummaryDurationPairs 耗时/首字节是**配对**统计：平均值必须由 sum ÷ count 算出，
// 故 count 的语义要严格（duration 记 >=0 的有效观测，ttfb 只记 >0 的观测）。
func TestSummaryDurationPairs(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	// 三条：100ms/30ms（都有效）、0ms（有效观测，亚毫秒）、-5ms（时钟回拨等脏数据，排除）。
	l.Record(Entry{UID: "u1", Status: 200, DurationMS: 100, TTFBMS: 30})
	l.Record(Entry{UID: "u1", Status: 200, DurationMS: 0, TTFBMS: 0})
	l.Record(Entry{UID: "u1", Status: 200, DurationMS: -5, TTFBMS: -1})

	s := l.Summary()
	if s.DurationSumMS != 100 || s.TimedCount != 2 {
		t.Errorf("duration sum/count=%d/%d want 100/2（0 是有效观测，负值排除）", s.DurationSumMS, s.TimedCount)
	}
	if s.TTFBSumMS != 30 || s.TTFBCount != 1 {
		t.Errorf("ttfb sum/count=%d/%d want 30/1", s.TTFBSumMS, s.TTFBCount)
	}
	if s.Requests != 3 {
		t.Errorf("requests=%d want 3（脏数据仍是一次调用，只是不计入耗时统计）", s.Requests)
	}
}

// TestSummaryConcurrentWithRecord 统计读取与记录写入并发时必须**内部自洽**：
// 控制台轮询 Summary 与请求出口的 Record 在真实运行中就是同时发生的。
//
// 关注的不是"数字准不准"（扫描期间的写入本来就可能不计入本次快照），
// 而是"读出来的快照不能是撕裂的"：分组求和必须等于合计、
// success+failed 必须等于 requests。撕裂快照会让控制台显示对不上的账。
func TestSummaryConcurrentWithRecord(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	used := float64(1)
	write := Entry{UID: "u1", Realm: "ai", Model: "m", Status: 200, PromptTokens: 10,
		CreditsKnown: true, CreditsUsed: &used}
	// 先写一条，避免读者在文件尚未创建时反复走空路径。
	l.Record(write)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 写入过程中穿插一次清空：让"清空使缓存失效"的路径也被并发覆盖。
		for i := 0; i < 200; i++ {
			l.Record(write)
			if i == 100 {
				l.Clear()
			}
		}
		close(stop)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := l.Summary()
			if s.Success+s.Failed != s.Requests {
				t.Errorf("torn snapshot: success(%d)+failed(%d) != requests(%d)", s.Success, s.Failed, s.Requests)
			}
			var acc Totals
			for _, g := range s.ByUID {
				acc.AddTotals(g.Totals)
			}
			for _, g := range s.ByRealm {
				acc.AddTotals(g.Totals)
			}
			// uid 与 realm 两个维度各贡献一份 requests/…，故合计应为 2×。
			if acc.Requests != 2*s.Requests {
				t.Errorf("torn snapshot: group sum %d != 2×requests %d", acc.Requests, 2*s.Requests)
			}
			if s.ByUID == nil || s.ByRealm == nil || s.ByModel == nil {
				t.Errorf("group slices must never be nil: %+v", s)
			}
		}
	}()
	wg.Wait()

	// 收尾：并发结束后快照必须与文件内容一致（缓存不会残留中间态）。
	l.Record(write)
	want := countFileRecords(t, fp)
	if got := int(l.Summary().Requests); got != want {
		t.Errorf("final summary requests=%d want %d (file record count)", got, want)
	}
}

// TestSummaryTimeFieldsAbsentWhenEmpty 从未有记录时 first_time/last_time 必须是 **null**，
// 绝不能是 "0001-01-01T00:00:00Z"。
//
// 这是一个真实踩过的坑：time.Time 是 struct，encoding/json 的 omitempty 对 struct
// 一律不生效（isEmptyValue 只处理 array/map/slice/string/bool/数值/指针/接口），
// 零值会被序列化成公元 1 年——前端 new Date() 照单全收，于是"一条记录都没有"
// 显示成「统计范围：01/01 08:00 起」，把不存在的时间当成事实。故这两类字段用指针。
// 断言"键存在但值为 null"：既锁住"能区分空"，也锁住字段名不因改名而悄悄消失。
func TestSummaryTimeFieldsAbsentWhenEmpty(t *testing.T) {
	cases := []struct {
		name string
		l    *Logger
	}{
		{"memory-only", New(Config{Enabled: true, MemorySize: 4})},
		{"disabled", New(Config{Enabled: false, File: filepath.Join(t.TempDir(), "u.jsonl")})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.l.Summary())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, k := range []string{"first_time", "last_time"} {
				v, has := m[k]
				if !has {
					t.Errorf("%s: key missing entirely: %s", tc.name, raw)
					continue
				}
				if v != nil {
					t.Errorf("%s: %s=%v want null（零值时间被序列化成公元 1 年会让前端显示假日期）", tc.name, k, v)
				}
			}
		})
	}

	// 清空之后同样回 null。
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	writeEntries(t, l, 2, nil)
	if l.Summary().FirstTime == nil {
		t.Fatal("fixture should have a time range")
	}
	l.Clear()
	if s := l.Summary(); s.FirstTime != nil || s.LastTime != nil {
		t.Errorf("after Clear first/last=%v/%v want nil", s.FirstTime, s.LastTime)
	}
}

// TestSummaryCreditsUnknownExcludesFailed 失败请求不计入"未记录消耗"：
// 失败本来就不产生消耗（见 internal/server/logging.go：402/429/5xx 一律记 null），
// 混进来会让一次 429/5xx 风暴把"有多少成功调用没记上账"这个真正的额度下界告警淹没。
func TestSummaryCreditsUnknownExcludesFailed(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	l := New(Config{Enabled: true, File: fp})
	l.Record(Entry{UID: "u1", Status: 200}) // 成功但无消耗数据 → 真正的下界告警
	l.Record(Entry{UID: "u1", Status: 429}) // 失败档
	l.Record(Entry{UID: "u1", Status: 503}) // 失败档

	s := l.Summary()
	if s.CreditsUnknown != 1 {
		t.Errorf("credits_unknown=%d want 1（只有成功调用才算'没记上账'）", s.CreditsUnknown)
	}
	if s.CreditsFailedUnknown != 2 {
		t.Errorf("credits_failed_unknown=%d want 2（失败档如实记录，但不混进告警）", s.CreditsFailedUnknown)
	}
	if s.Success != 1 || s.Failed != 2 {
		t.Errorf("success/failed=%d/%d want 1/2", s.Success, s.Failed)
	}
}

// TestSummaryIncompleteScanNotCached 单行超长导致扫描**不完整**时，结果不得进缓存。
//
// 缓存只按 (gen,total) 判活，而两者在"没有新记录"的窗口里都不变——恰是夜间空闲
// 这类最需要正确数字的场景。若残缺结果被缓存，少算的数字会一直留在页面上且无从察觉。
// 这里用"外部替换文件但不改 gen/total"来制造缓存键看不见的变化：
// 第一次扫描因超长行而中断（结果不完整），第二次必须重扫并看到替换后的完整内容。
func TestSummaryIncompleteScanNotCached(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	// 一行超过 summaryMaxScanBufBytes(1MB) 的脏数据：正常写入路径被 maxEntryBytes(64KB)
	// 限住，所以这只可能来自外部破坏——正是要求"如实标记不完整"的场景。
	huge := `{"uid":"u1","status":200,"model":"` + strings.Repeat("x", summaryMaxScanBufBytes+1024) + `"}`
	good := `{"uid":"u1","status":200,"prompt_tokens":7}`
	// 超长行放在**后面**：Scanner 读到它才终止，故 good 已被计入 → 统计不完整但非空。
	if err := os.WriteFile(fp, []byte(good+"\n"+huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Config{Enabled: true, File: fp})

	if first := l.Summary(); first.Requests != 1 {
		t.Fatalf("fixture: requests=%d want 1（超长行之前的记录仍应被统计）", first.Requests)
	}

	// 换成两条完好记录，**不动 gen/total**：只有"不缓存不完整结果"才可能看到新值。
	if err := os.WriteFile(fp, []byte(good+"\n"+good+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if second := l.Summary(); second.Requests != 2 {
		t.Errorf("second summary requests=%d want 2（不完整的扫描结果不得进缓存，否则陈旧数字会长期驻留）", second.Requests)
	}
}

// TestSummaryScanIncompleteFlag 扫描不完整时必须在 summary 里**如实上报**：
// 少算的数字在形态上与完整的一模一样，没有这个标记运维无法分辨
// "真的只用了这么多"和"这次少读了一段"。
func TestSummaryScanIncompleteFlag(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "usage.jsonl")
	huge := `{"uid":"u1","status":200,"model":"` + strings.Repeat("y", summaryMaxScanBufBytes+1024) + `"}`
	good := `{"uid":"u1","status":200,"prompt_tokens":7}`
	if err := os.WriteFile(fp, []byte(good+"\n"+huge+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Config{Enabled: true, File: fp})
	if s := l.Summary(); !s.ScanIncomplete {
		t.Error("scan_incomplete must be true when a line exceeds the scan buffer")
	}

	// 完好的文件必须报 false（否则这个标记就没有信息量）。
	fp2 := filepath.Join(t.TempDir(), "ok.jsonl")
	l2 := New(Config{Enabled: true, File: fp2})
	writeEntries(t, l2, 2, nil)
	if s := l2.Summary(); s.ScanIncomplete {
		t.Error("scan_incomplete must be false for a healthy file")
	}
	if s := emptySummary(); s.ScanIncomplete {
		t.Error("empty summary must not claim an incomplete scan")
	}
}
