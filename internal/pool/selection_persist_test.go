package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestAccountSelectionPersisted 账号级选号配置随 state.json 往返：
// 重启后排除 / 落位 / 优先级都必须还在（否则运维配的"最优先使用"会静默失效）。
func TestAccountSelectionPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "vip"})
	p.Add(&auth.Auth{UID: "backup"})
	if !p.SetAccountSelection("vip", AccountSelection{Excluded: true, Placement: PlacementFirst, Priority: 3}) {
		t.Fatal("set vip failed")
	}
	if !p.SetAccountSelection("backup", AccountSelection{Excluded: true, Placement: PlacementLast}) {
		t.Fatal("set backup failed")
	}
	p.Flush()

	p2 := New(fp)
	p2.SyncToDir([]*auth.Auth{{UID: "vip"}, {UID: "backup"}})
	vip, ok := p2.AccountSelection("vip")
	if !ok || !vip.Excluded || vip.Placement != PlacementFirst || vip.Priority != 3 {
		t.Fatalf("vip 配置未往返: %+v ok=%v", vip, ok)
	}
	backup, _ := p2.AccountSelection("backup")
	if !backup.Excluded || backup.Placement != PlacementLast {
		t.Fatalf("backup 配置未往返: %+v", backup)
	}

	// 落盘后的分组口径也必须一致：重启后 vip 仍是优先组、backup 仍是兜底组。
	withNoPickGap(t)
	p2.SetSelectionMode(SelectionWeighted)
	if got := p2.Pick(); got == nil || got.UID != "vip" {
		t.Fatalf("重启后 pick=%v want vip（优先组落位未持久化）", got)
	}
}

// TestAccountSelectionBackwardCompatibleStateFile 旧 state.json（无选号配置字段）
// 加载后一律零值：不排除、优先级 0，行为与改造前逐字一致。
func TestAccountSelectionBackwardCompatibleStateFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 模拟旧版落盘内容：只有历史字段，没有 selection_* 三个键。
	legacy := `{"accounts":{"u1":{"credits":100,"credits_known":true,"cool_kind":0},"u2":{"credits":5,"credits_known":true,"cool_kind":0}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	for _, uid := range []string{"u1", "u2"} {
		sel, ok := p.AccountSelection(uid)
		if !ok || sel.Excluded || sel.Placement != "" || sel.Priority != 0 {
			t.Fatalf("%s 旧状态应读到零值配置: %+v ok=%v", uid, sel, ok)
		}
	}
	// 零值配置下 lowest_credits 行为不变：选余额最小的 u2。
	withNoPickGap(t)
	p.SetSelectionMode(SelectionLowestCredits)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2（旧状态兼容后行为应不变）", got)
	}
}

// TestAccountSelectionOmittedFromStateWhenUnset 未配置的账号不该在 state.json 里
// 多出三个空字段：保持旧文件格式整洁，避免每次落盘都制造无意义 diff。
func TestAccountSelectionOmittedFromStateWhenUnset(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "plain"})
	p.SetCredits("plain", 10)
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var sf struct {
		Accounts map[string]map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"selection_excluded", "selection_placement", "selection_priority"} {
		if _, ok := sf.Accounts["plain"][key]; ok {
			t.Errorf("未配置的账号不该落盘 %s（omitempty 失效）: %s", key, raw)
		}
	}

	// 配置过之后必须落盘（否则重启即丢）。
	if !p.SetAccountSelection("plain", AccountSelection{Excluded: true, Placement: PlacementFirst, Priority: 2}) {
		t.Fatal("set failed")
	}
	p.Flush()
	raw, err = os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"selection_excluded", "selection_placement", "selection_priority"} {
		if _, ok := sf.Accounts["plain"][key]; !ok {
			t.Errorf("已配置的账号必须落盘 %s: %s", key, raw)
		}
	}
}

// TestSnapshotRestoreCarriesAccountSelection Redis 快照恢复同样要带上选号配置
// （快照与本地 state.json 同源，不能只有本地路径支持）。
func TestSnapshotRestoreCarriesAccountSelection(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "vip"})
	p.SetAccountSelection("vip", AccountSelection{Excluded: true, Placement: PlacementFirst, Priority: 11})
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var snap snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	// 直接把快照喂回 applySnapshotLocked（模拟 Redis 快照恢复路径）。
	p2 := New("")
	p2.mu.Lock()
	p2.applySnapshotLocked(snap)
	p2.mu.Unlock()
	sel, ok := p2.AccountSelection("vip")
	if !ok || !sel.Excluded || sel.Placement != PlacementFirst || sel.Priority != 11 {
		t.Fatalf("快照恢复未带上选号配置: %+v ok=%v", sel, ok)
	}
}
