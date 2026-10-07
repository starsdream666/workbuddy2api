package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/settings"
)

// TestSettingsSelectionModeLabelsAndOptions 选号策略在设置快照里的对外契约：
//   - Options 是配置里能写的**英文取值**（协议，四个都要在）；
//   - Labels 是控制台显示用的**中文文案**（每个取值都要有，否则下拉会露出机器词）。
//
// 这条测试锁的是"页面文案改成中文"与"配置文件格式不变"两件事同时成立。
func TestSettingsSelectionModeLabelsAndOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := newSettingsStore(path, cfg).Read()
	if err != nil {
		t.Fatal(err)
	}

	var field *settings.Field
	for i := range snap.Fields {
		if snap.Fields[i].Key == "pool.selection_mode" {
			field = &snap.Fields[i]
			break
		}
	}
	if field == nil {
		t.Fatal("pool.selection_mode 不在设置目录里")
	}
	want := []string{"weighted", "lowest_credits", "highest_credits", "custom_priority"}
	if len(field.Options) != len(want) {
		t.Fatalf("options=%v want %v", field.Options, want)
	}
	for i, option := range want {
		if field.Options[i] != option {
			t.Fatalf("options[%d]=%q want %q（顺序即下拉顺序，不要改）", i, field.Options[i], option)
		}
	}
	labels := snap.Labels["pool.selection_mode"]
	if len(labels) != len(want) {
		t.Fatalf("labels=%v 必须覆盖全部取值", labels)
	}
	for _, option := range want {
		label := labels[option]
		if label == "" {
			t.Errorf("取值 %q 缺少中文文案", option)
		}
		// 文案必须是人话：不能等于机器取值本身。
		if label == option {
			t.Errorf("取值 %q 的文案未中文化", option)
		}
	}
}

// TestSettingsSelectionModeEnumRejectsUnknown 设置接口只接受目录里列出的四个取值：
// 第五个值（哪怕是池内能识别的别名）必须被挡在 400，避免"页面能存、池不认"的裂缝。
func TestSettingsSelectionModeEnumRejectsUnknown(t *testing.T) {
	_, store := isolatedSettings(t, `{}`)
	before, _ := store.Read()
	for _, bad := range []string{`"random"`, `"highest"`, `"priority"`, `"WEIGHTED"`} {
		if _, err := store.Save(before.Revision, change("pool.selection_mode", bad)); err == nil {
			t.Errorf("selection_mode=%s 应被拒绝", bad)
		}
	}
	// 四个合法值都必须能存。
	for _, ok := range []string{`"weighted"`, `"lowest_credits"`, `"highest_credits"`, `"custom_priority"`} {
		snap, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Save(snap.Revision, change("pool.selection_mode", ok)); err != nil {
			t.Errorf("selection_mode=%s 应被接受: %v", ok, err)
		}
	}
}

// TestSettingsSelectionModeEffectiveFallback 控制台"当前生效值"必须与池的真实行为一致：
// 四个合法值原样透出，其余（含空值/笔误）一律显示 weighted。
func TestSettingsSelectionModeEffectiveFallback(t *testing.T) {
	cases := map[string]string{
		"weighted":        "weighted",
		"lowest_credits":  "lowest_credits",
		"highest_credits": "highest_credits",
		"custom_priority": "custom_priority",
		"":                "weighted",
		"bogus":           "weighted",
	}
	for configured, want := range cases {
		cfg := Default()
		cfg.Pool.SelectionMode = configured
		if got := effectiveSettingsValues(cfg)["pool.selection_mode"]; got != want {
			t.Errorf("configured=%q effective=%v want %v", configured, got, want)
		}
	}
}

// TestConfigSelectionModeRoundTrip 配置加载：新策略值经 JSON 往返不丢，
// 且不需要 normalize 做任何改写（池内 default 分支负责回落）。
func TestConfigSelectionModeRoundTrip(t *testing.T) {
	for _, mode := range []string{"weighted", "lowest_credits", "highest_credits", "custom_priority"} {
		path := filepath.Join(t.TempDir(), "config.json")
		raw, _ := json.Marshal(map[string]any{"pool": map[string]any{"selection_mode": mode}})
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if cfg.Pool.SelectionMode != mode {
			t.Fatalf("selection_mode=%q want %q", cfg.Pool.SelectionMode, mode)
		}
	}
}
