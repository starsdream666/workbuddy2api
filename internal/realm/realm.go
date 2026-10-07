// Package realm 定义上游产品线（realm）的端点与出站客户端身份。
//
// 上游有两条**互相隔离**的产品线，账号与 token 不互通（cn ↔ 国际线同 token 跨线一律 401）：
//
//	cn         CodeBuddy CN：chat → copilot.tencent.com，billing/签到 → www.codebuddy.cn，OAuth platform=CLI
//	workbuddy  WorkBuddy AI：chat/billing → www.workbuddy.ai，OAuth platform=workbuddy-ai
//	           （即 WorkBuddy 桌面端 product.json 的 endpoint，isOversea=true；历史名 "ai" 仍被接受）
//
// 另有一条**路线别名**（route alias）：
//
//	codebuddy  CodeBuddy IDE 域（www.codebuddy.ai），与 workbuddy 同属国际线：
//	           同一份 token 在两个域都通过（同后端、同账号空间、同账单），
//	           仅 host 与归因不同，故 Profile.AuthRealm = workbuddy —— 复用 workbuddy 的账号池。
//
// 三条线协议同构（同一 CLI 内核：/v2/chat/completions、/v2/billing/meter/*、
// /v2/activity/growth/*、/v3/config 路径一致，SSE 分帧一致），差异收敛在 host、
// OAuth platform 参数与出站客户端指纹三处。
package realm

import "strings"

// realm 标识（归一名）。
const (
	// CN CodeBuddy 国内线（copilot.tencent.com + www.codebuddy.cn）。
	CN = "cn"
	// WB WorkBuddy AI 线（www.workbuddy.ai）。
	WB = "workbuddy"
	// AI 旧名（= "ai"）：历史 auth 文件 / 旧配置 / X-Realm: ai 仍然接受，
	// Normalize 之后一律变成 WB。新代码请用 WB。
	AI = "ai"
	// CB CodeBuddy IDE 国际域线（www.codebuddy.ai）：WB 的路线别名，复用 WB 账号池。
	CB = "codebuddy"
)

// Fingerprint 出站客户端指纹风格。上游按出站 UA/X-Product 做「使用端」归因，
// 同一 realm 可切这两种风格（见 config.upstream.realm_overrides）。
type Fingerprint string

const (
	// FingerprintCLI 官方 CLI 指纹：`CLI/<ver> CodeBuddy/<ver>` + `X-Product: SaaS`。
	// 与项目历史行为逐字一致，也是 CN 线默认。
	FingerprintCLI Fingerprint = "cli"
	// FingerprintDesktop WorkBuddy 桌面端指纹：`WorkBuddy/<ver>` + `X-Product: WorkBuddy`
	// + `X-IDE-Type/X-IDE-Name: WorkBuddy` + `X-IDE-Version: <ver>`。
	// 与桌面端 AuthService.getActivityBanner 的出站头逐字一致。
	FingerprintDesktop Fingerprint = "workbuddy-desktop"
)

// Profile 一条产品线的端点与默认身份。
type Profile struct {
	Name string
	// ChatBase 聊天补全 / token 刷新 / OAuth / 模型列表的 base（无尾斜杠）。
	ChatBase string
	// BillingBase 签到 / 余额 / 活跃上报的 base（无尾斜杠）。
	BillingBase string
	// Platform OAuth 请求的 platform 查询参数值。
	Platform string
	// Origin Origin / Referer 基址（无尾斜杠）。
	Origin string
	// Fingerprint 该 realm 的默认出站指纹（可被配置覆盖）。
	Fingerprint Fingerprint
	// CLIVersion 该 realm 的 CLI 内核版本（cli 指纹用）。
	CLIVersion string
	// DesktopVersion WorkBuddy 桌面端版本（desktop 指纹用）。
	DesktopVersion string
	// AuthRealm 凭证来源线（"路线别名"用）：非空且不等于自身时，该 realm 复用来源线的
	// 账号池与凭证（账号文件里的 realm 不变），只有上游 base/归因按本 realm 的档案走。
	// 空 = 自身就是一条独立产品线。
	AuthRealm string
}

// cnProfile CodeBuddy 国内线（与改造前的硬编码常量逐字一致）。
var cnProfile = Profile{
	Name:           CN,
	ChatBase:       "https://copilot.tencent.com",
	BillingBase:    "https://www.codebuddy.cn",
	Platform:       "CLI",
	Origin:         "https://www.codebuddy.cn",
	Fingerprint:    FingerprintCLI,
	CLIVersion:     "2.63.2",
	DesktopVersion: "",
}

// wbProfile WorkBuddy AI 线（realm 名 workbuddy；历史名 "ai" 仍被 Normalize 接受）。
//
// 端点取自桌面端 product.json：endpoint=https://www.workbuddy.ai，
// authentication.id=workbuddy-desktop-ai（platform 参数 workbuddy-ai）；
// chat 与 billing 同域（实测 /v2/billing/meter/get-user-resource 在该域 200）。
// 默认指纹保持 cli（历史行为、零新增指纹），需要按桌面端归因时配置切 desktop。
var wbProfile = Profile{
	Name:           WB,
	ChatBase:       "https://www.workbuddy.ai",
	BillingBase:    "https://www.workbuddy.ai",
	Platform:       "workbuddy-ai",
	Origin:         "https://www.workbuddy.ai",
	Fingerprint:    FingerprintCLI,
	CLIVersion:     "2.63.2",
	DesktopVersion: "5.5.2",
}

// cbProfile CodeBuddy IDE 国际域线（workbuddy 的路线别名）。
//
// 端点取自 CodeBuddy IDE product.json：endpoint=https://www.codebuddy.ai；
// 与 workbuddy 同后端（/v3/config 的 data 段逐字段一致、billing 返回同一钱包），
// 故 AuthRealm=workbuddy —— 直接复用 workbuddy 的已有账号，无需重新登录、无需迁移凭证文件。
// platform 仍用 workbuddy-ai（同一产品空间；IDE 自己的 OAuth platform=ide，
// 可在 upstream.realm_overrides.codebuddy.platform 覆盖）。
var cbProfile = Profile{
	Name:           CB,
	ChatBase:       "https://www.codebuddy.ai",
	BillingBase:    "https://www.codebuddy.ai",
	Platform:       "workbuddy-ai",
	Origin:         "https://www.codebuddy.ai",
	Fingerprint:    FingerprintCLI,
	CLIVersion:     "2.63.2",
	DesktopVersion: "5.5.2",
	AuthRealm:      WB,
}

// Defaults 全部内置档案（返回副本，调用方可安全改写）。
// 键是**归一名**（cn / workbuddy / codebuddy）；旧名 "ai" 请先过 Normalize。
func Defaults() map[string]Profile {
	return map[string]Profile{CN: cnProfile, WB: wbProfile, CB: cbProfile}
}

// Known 报告 name 是否是已知 realm（空串视为已知：归一为 CN）。
// 注意不能直接对 Normalize 的结果做判断——Normalize 把未知值也归一到 CN，
// 那样 Known 会恒为 true，X-Realm 头的合法校验就形同虚设。
// 旧名 "ai" 仍算已知（兼容），只是会被归一成 workbuddy。
func Known(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case CN, WB, AI, CB, "":
		return true
	default:
		return false
	}
}

// Normalize 归一 realm 名：
//
//	workbuddy / ai（旧名）→ workbuddy
//	codebuddy             → codebuddy
//	cn / 空 / 未知        → cn（改造前唯一行为：未知值回落 CN）
//
// 旧名 "ai" 保留兼容：历史 auth 文件（realm=ai）、旧配置（realms.ai / realm_overrides.ai /
// schedule.realm_tasks.ai）与 X-Realm: ai 都继续工作，运行时统一按 workbuddy 处理。
func Normalize(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case WB, AI:
		return WB
	case CB:
		return CB
	case CN, "":
		return CN
	default:
		return CN
	}
}

// All 全部已知 realm（稳定顺序，用于日志与状态展示）。
func All() []string { return []string{CN, WB, CB} }

// AuthRealmOf 返回该 realm 的凭证来源线（无别名时为自身）。
// 主干判定之一：pool 归属按它解析——别名 realm 不建自己的池，直接复用来源线的池。
func AuthRealmOf(name string) string {
	rn := Normalize(name)
	if src := strings.TrimSpace(Defaults()[rn].AuthRealm); src != "" {
		return Normalize(src)
	}
	return rn
}

// IsRouteAlias 报告该 realm 是否是路线别名（凭证来自另一条线）。
func IsRouteAlias(name string) bool { return AuthRealmOf(name) != Normalize(name) }

// IsIntlLine 报告该 realm 是否属于国际线（workbuddy / codebuddy）。
// 国际线的共同行为：首条消息必须为 system、模型表以服务端下发的 /v3/config 为准。
func IsIntlLine(name string) bool {
	switch Normalize(name) {
	case WB, CB:
		return true
	default:
		return false
	}
}

// NormalizeFingerprint 归一指纹名；未知 / 空返回 false（调用方保留默认）。
func NormalizeFingerprint(name string) (Fingerprint, bool) {
	switch Fingerprint(strings.ToLower(strings.TrimSpace(name))) {
	case FingerprintCLI:
		return FingerprintCLI, true
	case FingerprintDesktop:
		return FingerprintDesktop, true
	default:
		return "", false
	}
}

// ClientInfo 出站客户端身份（头部与 UA 的取值来源）。
type ClientInfo struct {
	UserAgent string
	// Product X-Product 头取值（CLI 风格为 SaaS，桌面风格为 WorkBuddy）。
	Product string
	// IDEType/IDEName/IDEVersion 仅桌面指纹非空（X-IDE-* 三头）。
	IDEType    string
	IDEName    string
	IDEVersion string
}

// ClientInfo 按指纹风格构造身份；version 为空时回落 profile 内置版本。
func (p Profile) ClientInfo(fp Fingerprint, version string) ClientInfo {
	v := strings.TrimSpace(version)
	if fp == FingerprintDesktop {
		if v == "" {
			v = p.DesktopVersion
		}
		return ClientInfo{
			UserAgent:  "WorkBuddy/" + v,
			Product:    "WorkBuddy",
			IDEType:    "WorkBuddy",
			IDEName:    "WorkBuddy",
			IDEVersion: v,
		}
	}
	if v == "" {
		v = p.CLIVersion
	}
	return ClientInfo{
		UserAgent: "CLI/" + v + " CodeBuddy/" + v,
		Product:   "SaaS",
	}
}
