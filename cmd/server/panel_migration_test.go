package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPanelFeatureDefaultsAndOverrides(test *testing.T) {
	defaults := Default()
	if defaults.Pool.CreditFloor != 0 {
		test.Fatal("credit floor must default to disabled")
	}
	if !defaults.Features.PromptCacheKey || defaults.Features.RepairToolHistory {
		test.Fatal("unsafe feature defaults")
	}
	path := filepath.Join(test.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"features":{"prompt_cache_key":false,"repair_tool_history":true}}`), 0o600); err != nil {
		test.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		test.Fatal(err)
	}
	if loaded.Features.PromptCacheKey || !loaded.Features.RepairToolHistory {
		test.Fatal("file override lost")
	}
	test.Setenv("WB2A_PROMPT_CACHE_KEY", "true")
	test.Setenv("WB2A_REPAIR_TOOL_HISTORY", "false")
	loaded, err = Load(path)
	if err != nil || !loaded.Features.PromptCacheKey || loaded.Features.RepairToolHistory {
		test.Fatal("environment override lost")
	}
}

func TestCreditFloorConfig(test *testing.T) {
	path := filepath.Join(test.TempDir(), "config.json")
	for _, value := range []struct {
		body      string
		wantError bool
		floor     float64
	}{
		{`{"pool":{"credit_floor":12.5}}`, false, 12.5},
		{`{"pool":{"credit_floor":-1}}`, true, 0},
	} {
		if err := os.WriteFile(path, []byte(value.body), 0o600); err != nil {
			test.Fatal(err)
		}
		loaded, err := Load(path)
		if (err != nil) != value.wantError {
			test.Fatalf("load error=%v", err)
		}
		if err == nil && loaded.Pool.CreditFloor != value.floor {
			test.Fatal("credit floor not loaded")
		}
	}
}
