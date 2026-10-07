// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/realm"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu          sync.Mutex
	deleted     bool // 控制台删除后禁止在途 token 刷新重新写回文件。
	refreshOnce sync.Once
	refreshGate chan struct{}

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒（磁盘上允许毫秒，Parse 时归一）
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	// Realm 账号所属上游产品线（归一名 "cn" / "workbuddy" / "codebuddy"）；空 = 未标注，
	// 由调用方按配置默认值归一。旧名 "ai" 会被 realm.Normalize 归一成 "workbuddy"。
	// 同 token 跨线必被上游 401，故池选号必须按 realm 分区。
	Realm    string
	FilePath string // 来源文件；refresh 后原子写回此处
}

// NormalizeExpiresAt 归一过期时间到 Unix 秒。
// 上游/桌面端凭证两套口径并存：CLI OAuth 落秒，WorkBuddy 桌面端 auth 落毫秒
// （实测 expiresAt=1820653886152）。以 1e12 为界（秒口径要到公元 33658 年才越过），
// 越过即按毫秒除以 1000，避免毫秒当秒用导致 token 被判定"永不过期"。
func NormalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Realm string `json:"realm"`
			Auth  struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		realmName := n.Auth.Realm
		if realmName == "" {
			realmName = n.Realm
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    NormalizeExpiresAt(n.Auth.ExpiresAt),
			Domain:       n.Auth.Domain,
			Realm:        realmName,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			Realm        string `json:"realm"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    NormalizeExpiresAt(f.ExpiresAt),
			Domain:       f.Domain,
			Realm:        f.Realm,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
// realm 非空才落盘（旧文件保持原样，不被网关单方面标注产品线）。
// expiresAt 统一按 Unix 秒写回（磁盘上毫秒形态在 Parse 时已归一）。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted {
		return fmt.Errorf("save refused: credential deleted (uid=%s)", a.UID)
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	if r := strings.TrimSpace(a.Realm); r != "" {
		doc["realm"] = r
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// DeleteFile 与 SaveAtomic 串行，删除成功后阻止同一凭证对象被在途刷新重新落盘。
// 路径边界由管理接口验证；文件已不存在视为幂等成功。
func (a *Auth) DeleteFile() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.deleted = true
	return nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// FilePrefixFor realm 的凭证文件名前缀（login.sh / 控制台 / importauth 三处共用同一规则）：
//
//	cn        → workbuddy     （cn 的凭证一直是 workbuddy-<uid>.json）
//	workbuddy → workbuddy-ai  （历史命名；realm 改名 ai → workbuddy 后沿用，避免与 cn 的文件名撞车）
//	其他      → workbuddy-<realm>
//
// 文件名只是历史习惯：池归属按文件里的 realm 字段判定（见 LoadDir）。
func FilePrefixFor(rn string) string {
	switch realm.Normalize(rn) {
	case realm.CN:
		return "workbuddy"
	case realm.WB:
		return "workbuddy-" + realm.AI // 历史名 "ai"
	default:
		return "workbuddy-" + realm.Normalize(rn)
	}
}

// AuthFileFor 凭证落盘路径（dir 为空回落 ./auths）。
func AuthFileFor(dir, rn, uid string) string {
	if !ValidUID(uid) {
		return ""
	}
	if strings.TrimSpace(dir) == "" {
		dir = "./auths"
	}
	return filepath.Join(dir, FilePrefixFor(rn)+"-"+uid+".json")
}
