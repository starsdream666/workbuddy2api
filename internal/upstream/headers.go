// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6，realm 维度见 internal/realm。
package upstream

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

const (
	// clientUA CN 线默认出站 UA（= realm.Defaults()[cn] + cli 指纹的等价字面量）。
	// 保持常量以便测试与文档引用；实际取值走 profile + 指纹解析。
	clientUA        = "CLI/2.168.2 CodeBuddy/2.168.2"
	originRefererCN = "https://www.codebuddy.cn"
)

// realmOf 解析本次出站应使用的 realm 档案：
//  1. Client.RouteRealm（路线别名：codebuddy 借 ai 账号时按别名线出站）
//  2. auth 自带 realm
//  3. Client.RealmDefault（空 = cn）
func (c *Client) realmOf(a *auth.Auth) string {
	if c != nil && strings.TrimSpace(c.RouteRealm) != "" {
		return realm.Normalize(c.RouteRealm)
	}
	if a != nil && strings.TrimSpace(a.Realm) != "" {
		return realm.Normalize(a.Realm)
	}
	if c != nil {
		return realm.Normalize(c.RealmDefault)
	}
	return realm.CN
}

// profileOf 取账号所属 realm 的端点档案：内置档案 + Client.Profiles 覆盖
// + CN 历史字段（ChatBaseCN/BillingBaseCN，测试与旧调用方仍可注入）。
func (c *Client) profileOf(a *auth.Auth) realm.Profile {
	name := c.realmOf(a)
	p := realm.Defaults()[name]
	if c != nil {
		if ov, ok := c.Profiles[name]; ok {
			if ov.ChatBase != "" {
				p.ChatBase = ov.ChatBase
			}
			if ov.BillingBase != "" {
				p.BillingBase = ov.BillingBase
			}
			if ov.Origin != "" {
				p.Origin = ov.Origin
			}
			if ov.Platform != "" {
				p.Platform = ov.Platform
			}
			if ov.Fingerprint != "" {
				p.Fingerprint = ov.Fingerprint
			}
			if ov.CLIVersion != "" {
				p.CLIVersion = ov.CLIVersion
			}
			if ov.DesktopVersion != "" {
				p.DesktopVersion = ov.DesktopVersion
			}
		}
		if name == realm.CN {
			if c.ChatBaseCN != "" {
				p.ChatBase = c.ChatBaseCN
			}
			if c.BillingBaseCN != "" {
				p.BillingBase = c.BillingBaseCN
			}
		}
	}
	return p
}

// ProfileFor 返回指定 realm 的生效档案（端点 base / OAuth platform / 指纹 / 版本）。
// 供控制台与管理工具复用同一套 realm 解析，避免两处漂移。
func (c *Client) ProfileFor(rn string) realm.Profile {
	return c.profileOf(&auth.Auth{Realm: realm.Normalize(rn)})
}

// fingerprintOf 取该 realm 生效的指纹风格：显式配置优先，否则 profile 默认。
func (c *Client) fingerprintOf(a *auth.Auth) realm.Fingerprint {
	p := c.profileOf(a)
	if c != nil {
		if fp, ok := c.RealmFingerprints[c.realmOf(a)]; ok && fp != "" {
			return fp
		}
	}
	return p.Fingerprint
}

// clientInfoOf 取该账号出站客户端身份（UA + X-Product + X-IDE-*）。
func (c *Client) clientInfoOf(a *auth.Auth) realm.ClientInfo {
	p := c.profileOf(a)
	version := ""
	if c != nil {
		version = c.RealmVersions[c.realmOf(a)]
	}
	return p.ClientInfo(c.fingerprintOf(a), version)
}

// userAgent 返回 CN 默认出站 UA（无账号上下文的历史入口，供文档/测试引用）。
func (c *Client) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return clientUA
}

// userAgentFor 返回该账号的出站 UA。
// 优先级：Client.UserAgent（全局显式覆盖，历史语义）> 该 realm 的 UA 覆盖 >
// 该 realm 指纹身份（cli / workbuddy-desktop，可配）。
func (c *Client) userAgentFor(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	if c != nil {
		if ua, ok := c.RealmUserAgents[c.realmOf(a)]; ok && strings.TrimSpace(ua) != "" {
			return ua
		}
	}
	return c.clientInfoOf(a).UserAgent
}

// billingUAFor 返回 billing 路径的 UA：默认**不设置**（保持现状，Go 客户端自带默认 UA），
// 仅当全局 UserAgent 非空、或该 realm 被显式配置了指纹/版本时才覆盖——
// 避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) billingUAFor(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	if c == nil {
		return ""
	}
	name := c.realmOf(a)
	if _, ok := c.RealmFingerprints[name]; ok {
		return c.userAgentFor(a)
	}
	if ua, ok := c.RealmUserAgents[name]; ok && strings.TrimSpace(ua) != "" {
		return ua
	}
	if v := strings.TrimSpace(c.RealmVersions[name]); v != "" {
		return c.userAgentFor(a)
	}
	p := realm.Defaults()[name]
	if ov, ok := c.Profiles[name]; ok {
		if ov.Fingerprint != "" || ov.DesktopVersion != "" || ov.CLIVersion != "" {
			return p.ClientInfo(ov.Fingerprint, "").UserAgent
		}
	}
	return ""
}

// CommonHeaders 设置所有 API 共享的请求头（realm 决定 Origin/Referer 与 UA）。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// X-CodeBuddy-Request 是官方定义的**安全请求头**：值固定 "1"，除豁免路径外
	// 所有 API 请求都必须携带（官方 SDK 的 CustomRequestHeader schema 原文：
	// 「安全请求头，值固定为 `1`。所有 API 请求（除豁免路径外）必须携带」）。
	// 本项目此前完全不发它 —— 这是最直白的"非官方来源"缺口。
	req.Header.Set("X-CodeBuddy-Request", "1")
	origin := c.profileOf(a).Origin
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgentFor(a))
}

// applyClientIdentity 附加客户端身份头：X-Product 恒设（CN+cli 即历史值 "SaaS"），
// X-IDE-* 仅在桌面指纹（客户端自报 IDE 身份）时出现。
func (c *Client) applyClientIdentity(req *http.Request, a *auth.Auth) {
	ci := c.clientInfoOf(a)
	if ci.Product != "" {
		req.Header.Set("X-Product", ci.Product)
	}
	if ci.IDEType != "" {
		req.Header.Set("X-IDE-Type", ci.IDEType)
	}
	if ci.IDEName != "" {
		req.Header.Set("X-IDE-Name", ci.IDEName)
	}
	if ci.IDEVersion != "" {
		req.Header.Set("X-IDE-Version", ci.IDEVersion)
	}
}

// applyAgentIdentity 附加官方 agent 头族（X-Agent-Intent / Purpose / Type / Runtime）。
//
// 依据：官方 WorkBuddy 桌面客户端 application-manifest.js 在每条 chat 上都带
// X-Agent-Intent: craft + X-Agent-Purpose: conversation（国际线另带 X-Agent-Type: main）；
// 官方 CLI 包 2.168.2 的头常量表定义同一族，X-Agent-Runtime 取值域
// [local-agent, cloud-agent, agent-agent]。缺少这一族时上游把请求归到"没有客户端"，
// 官网「使用端」列显示为空 —— 这是可见的身份缺口，不是猜测。
// 字段为空就不发（宁可少发，也不伪造自己没有依据的取值）。
func (c *Client) applyAgentIdentity(req *http.Request, a *auth.Auth) {
	ci := c.clientInfoOf(a)
	if ci.AgentIntent != "" {
		req.Header.Set("X-Agent-Intent", ci.AgentIntent)
	}
	if ci.AgentPurpose != "" {
		req.Header.Set("X-Agent-Purpose", ci.AgentPurpose)
	}
	if ci.AgentType != "" {
		req.Header.Set("X-Agent-Type", ci.AgentType)
	}
	if ci.AgentRuntime != "" {
		req.Header.Set("X-Agent-Runtime", ci.AgentRuntime)
	}
}


// applyConversationIdentity 设置官方桌面端的**会话身份三件套**：
// X-Conversation-ID / X-Conversation-Request-ID / X-Conversation-Message-ID。
//
// 依据（两条独立来源 + 本实例实号对照）：
//   - 官方客户端 bundle 的 applyGatewayHeaders 在每条 chat 上各生成一个 randomUUID；
//   - 同类项目 dsh-connect-workbuddy 在 4.6.3 把「缺三件套 → 11128」定为根因，
//     并给出最小必需集：单独一个 X-Conversation-ID 仍然 11128，三个都要。
//
// 本实例实测（同一账号、同一 body、同一套其余头，只增删这三行）：
//
//	uid=816db6e5 不带 2/2 被拦（400 code 11128 "request illegal"）→ 带 3/3 全部 200
//	uid=f4e1d05d 不带 2/2 被拦                                → 带 3/3 全部 200
//
// 也就是说此前**不是账号被上游标记**，而是请求缺会话身份：上游对缺失的容忍度
// 按账号不同，同一条请求在两个号上一个 400 一个 200，看起来才像账号级。
//
// 三个值上游不校验来源、只做关联，但**不能复用同一个值**（复用等于拿一个 id
// 冒充三次会话身份）。每次请求新生成，与官方客户端 randomUUID() 的行为一致。
//
// 只加在 chat：官方客户端调 billing / refresh 时不带，实测也不影响。
func (c *Client) applyConversationIdentity(req *http.Request) {
	req.Header.Set("X-Conversation-ID", newUUIDv4())
	req.Header.Set("X-Conversation-Request-ID", newUUIDv4())
	req.Header.Set("X-Conversation-Message-ID", newUUIDv4())
}
// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth) {
	a = a.Snapshot()
	c.CommonHeaders(req, a)
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	c.applyClientIdentity(req, a)
	// chat 的 Accept 按官方声明：官方 chat 请求带 text/event-stream（不是 */*），
	// 缺了会显得像个通用 OpenAI 客户端而不是 WorkBuddy 自己。
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.applyAgentIdentity(req, a)
	// 会话身份三件套 —— 国际网关在「首条必须是 system」之外的第二道闸：
	// 缺这三个头即 HTTP 400 + code 11128（msg 可能写 "request illegal"，也可能写
	// "conversation id missing"，displayTips 还会误写成"可能不是官方客户端发出的"）。
	c.applyConversationIdentity(req)
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// 全局 UA 或该 realm 的指纹/版本时才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	a = a.Snapshot()
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if ua := c.billingUAFor(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	a = a.Snapshot()
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
