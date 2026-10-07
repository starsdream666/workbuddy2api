package realm

import "testing"

// TestNormalizeRelm：空值/未知值回落 cn（改造前唯一行为），大小写与空白容错。
// 旧名 "ai" 归一为归一名 "workbuddy"。
func TestNormalizeRelm(t *testing.T) {
	cases := map[string]string{
		"":           CN,
		"   ":        CN,
		"cn":         CN,
		"CN":         CN,
		"workbuddy":  WB,
		"workbuddy ": WB,
		"ai":         WB,
		" AI ":       WB,
		"codebuddy":  CB,
		" CodeBuddy": CB,
		"unknown":    CN,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q)=%q want %q", in, got, want)
		}
	}
}

func TestKnown(t *testing.T) {
	if !Known("cn") || !Known("") {
		t.Error("cn/空 都应是已知 realm（空归一为 cn）")
	}
	if !Known(WB) || !Known("WorkBuddy") {
		t.Error("workbuddy 应是已知 realm")
	}
	if !Known("ai") || !Known(" AI ") {
		t.Error("旧名 ai 仍必须是已知 realm（历史 auth 文件 / 旧配置 / X-Realm: ai）")
	}
	if !Known("codebuddy") || !Known("CodeBuddy") {
		t.Error("codebuddy 应是已知 realm（路线别名）")
	}
	if Known("staging") {
		t.Error("未知 realm 不该被认同")
	}
}

// TestCNProfileMatchesLegacyConstants cn 档案必须与改造前的硬编码常量逐字一致
// （任何漂移都会改变既有 CN 部署的出站指纹）。
func TestCNProfileMatchesLegacyConstants(t *testing.T) {
	p := Defaults()[CN]
	if p.ChatBase != "https://copilot.tencent.com" {
		t.Errorf("ChatBase=%q", p.ChatBase)
	}
	if p.BillingBase != "https://www.codebuddy.cn" {
		t.Errorf("BillingBase=%q", p.BillingBase)
	}
	if p.Platform != "CLI" {
		t.Errorf("Platform=%q", p.Platform)
	}
	if p.Origin != "https://www.codebuddy.cn" {
		t.Errorf("Origin=%q", p.Origin)
	}
	ci := p.ClientInfo(p.Fingerprint, "")
	if ci.UserAgent != "CLI/2.63.2 CodeBuddy/2.63.2" {
		t.Errorf("UA=%q", ci.UserAgent)
	}
	if ci.Product != "SaaS" {
		t.Errorf("Product=%q", ci.Product)
	}
	if ci.IDEType != "" || ci.IDEName != "" || ci.IDEVersion != "" {
		t.Errorf("CLI 指纹不该带 X-IDE-* : %+v", ci)
	}
}

// TestWBProfileEndpoints：workbuddy 档案取自 WorkBuddy 桌面端 product.json。
func TestWBProfileEndpoints(t *testing.T) {
	p, ok := Defaults()[WB]
	if !ok {
		t.Fatalf("Defaults() 缺 %q（归一名）", WB)
	}
	if p.Name != WB {
		t.Errorf("Name=%q want %q", p.Name, WB)
	}
	if p.ChatBase != "https://www.workbuddy.ai" || p.BillingBase != "https://www.workbuddy.ai" {
		t.Errorf("workbuddy base=%q/%q", p.ChatBase, p.BillingBase)
	}
	if p.Platform != "workbuddy-ai" {
		t.Errorf("Platform=%q want workbuddy-ai", p.Platform)
	}
	if p.AuthRealm != "" {
		t.Errorf("AuthRealm=%q want 空（workbuddy 自身就是一条产品线）", p.AuthRealm)
	}
}

// TestLegacyAINameAlias 旧名 "ai" 在整个 realm 包的口径里都必须等价于 workbuddy：
// 改名只动名字，不动行为 —— 旧部署（auth 文件 realm=ai、配置 realms.ai、X-Realm: ai）零改动继续工作。
func TestLegacyAINameAlias(t *testing.T) {
	if AI != "ai" || WB != "workbuddy" {
		t.Fatalf("常量漂移: AI=%q WB=%q", AI, WB)
	}
	if Normalize(AI) != WB {
		t.Errorf("Normalize(ai)=%q want %q", Normalize(AI), WB)
	}
	if !Known(AI) {
		t.Error("旧名 ai 必须仍算已知 realm")
	}
	if AuthRealmOf(AI) != WB {
		t.Errorf("AuthRealmOf(ai)=%q want %q（自身线，归一名）", AuthRealmOf(AI), WB)
	}
	if IsRouteAlias(AI) {
		t.Error("ai 只是旧名，不是路线别名")
	}
	if !IsIntlLine(AI) {
		t.Error("旧名 ai 属于国际线（首条 system / 模型表走 /v3/config）")
	}
	if Defaults()[AI].ChatBase != "" {
		t.Error("Defaults() 的键只认归一名，旧名不该另有一份档案（否则两处会漂移）")
	}
}

// TestClientInfoDesktopFingerprint 桌面指纹与桌面端 AuthService 出站头一致。
func TestClientInfoDesktopFingerprint(t *testing.T) {
	p := Defaults()[WB]
	ci := p.ClientInfo(FingerprintDesktop, "")
	if ci.UserAgent != "WorkBuddy/"+p.DesktopVersion {
		t.Errorf("UA=%q want WorkBuddy/%s", ci.UserAgent, p.DesktopVersion)
	}
	if ci.Product != "WorkBuddy" || ci.IDEType != "WorkBuddy" || ci.IDEName != "WorkBuddy" {
		t.Errorf("desktop identity=%+v", ci)
	}
	if ci.IDEVersion != p.DesktopVersion {
		t.Errorf("IDEVersion=%q want %q", ci.IDEVersion, p.DesktopVersion)
	}
	// 显式版本覆盖
	if got := p.ClientInfo(FingerprintDesktop, "9.9.9").UserAgent; got != "WorkBuddy/9.9.9" {
		t.Errorf("override UA=%q", got)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	if fp, ok := NormalizeFingerprint("WorkBuddy-Desktop"); !ok || fp != FingerprintDesktop {
		t.Errorf("fp=%q ok=%v", fp, ok)
	}
	if fp, ok := NormalizeFingerprint("cli"); !ok || fp != FingerprintCLI {
		t.Errorf("fp=%q ok=%v", fp, ok)
	}
	if _, ok := NormalizeFingerprint("chrome"); ok {
		t.Error("未知指纹不该被认同")
	}
}

// TestCodebuddyAliasProfile：codebuddy 是 workbuddy 的路线别名 —— 复用 workbuddy 账号池，只换出站域。
func TestCodebuddyAliasProfile(t *testing.T) {
	p := Defaults()[CB]
	if p.ChatBase != "https://www.codebuddy.ai" || p.BillingBase != "https://www.codebuddy.ai" {
		t.Errorf("codebuddy base=%q/%q", p.ChatBase, p.BillingBase)
	}
	if p.Origin != "https://www.codebuddy.ai" {
		t.Errorf("Origin=%q want https://www.codebuddy.ai（Origin/Referer 必须与目标域一致）", p.Origin)
	}
	if p.Platform != "workbuddy-ai" {
		t.Errorf("Platform=%q want workbuddy-ai", p.Platform)
	}
	if p.AuthRealm != WB {
		t.Errorf("AuthRealm=%q want %q（凭证复用 workbuddy 线）", p.AuthRealm, WB)
	}
	if got := AuthRealmOf(CB); got != WB {
		t.Errorf("AuthRealmOf(codebuddy)=%q want %q", got, WB)
	}
	if got := AuthRealmOf(WB); got != WB {
		t.Errorf("AuthRealmOf(workbuddy)=%q want %q（非别名返回自身）", got, WB)
	}
	if got := AuthRealmOf(CN); got != CN {
		t.Errorf("AuthRealmOf(cn)=%q want %q", got, CN)
	}
	if !IsRouteAlias(CB) {
		t.Error("codebuddy 应是路线别名")
	}
	if IsRouteAlias(WB) || IsRouteAlias(CN) {
		t.Error("workbuddy / cn 不是路线别名")
	}
}

// TestIntlLine：国际线（workbuddy / codebuddy）共同行为 —— 首条必须 system、模型表走 /v3/config。
func TestIntlLine(t *testing.T) {
	if !IsIntlLine(WB) || !IsIntlLine(CB) {
		t.Error("workbuddy / codebuddy 都应属于国际线")
	}
	if IsIntlLine(CN) || IsIntlLine("") {
		t.Error("cn / 空值不该被当作国际线")
	}
	if IsIntlLine("zh-cn") || IsIntlLine("bogus") {
		t.Error("未知值不该被当作国际线")
	}
}

// TestAllRealms：三条线顺序稳定（日志与状态展示依赖它），且键都是归一名。
func TestAllRealms(t *testing.T) {
	got := All()
	want := []string{CN, WB, CB}
	if len(got) != len(want) {
		t.Fatalf("All()=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("All()=%v want %v", got, want)
		}
	}
	for _, rn := range want {
		if _, ok := Defaults()[rn]; !ok {
			t.Errorf("Defaults() 缺 realm %q", rn)
		}
	}
	// 旧名不在 All() 里（它是别名，不是第三条线），避免日志/状态里出现重复口径。
	for _, rn := range got {
		if rn == AI {
			t.Errorf("All() 不该含旧名 %q（应只出现归一名 %q）", AI, WB)
		}
	}
}
