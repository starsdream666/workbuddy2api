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
	clientUA        = "CLI/2.63.2 CodeBuddy/2.63.2"
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
