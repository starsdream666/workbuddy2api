// Package oauth 封装 WorkBuddy 设备授权流程（无 PKCE，state 由服务端签发），
// 供 CLI（cmd/login）与控制台（server 管理 API）共用：
//
//	Start(profile)          → POST {chatBase}/v2/plugin/auth/state?platform=<platform>
//	                          拿 state + authUrl（用户需在浏览器打开 authUrl 完成登录）
//	Poll(profile, state)    → GET /v2/plugin/auth/token?state= ；未完成返回 ErrPending
//	                          完成后 GET /v2/plugin/login/account?state= 拿 uid/nickname
//
// 两条产品线（realm）端点不同，全部由 realm.Profile 决定：
//
//	cn  chatBase=copilot.tencent.com，platform=CLI
//	ai  chatBase=www.workbuddy.ai，platform=workbuddy-ai
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"time"

	"workbuddy2api/internal/realm"
)

// ErrPending 登录尚未完成（上游 code != 0 / 缺 accessToken）。
var ErrPending = errors.New("login pending")

// Timeout 单次请求超时。
const Timeout = 30 * time.Second

// Session 一次待完成的授权会话。
type Session struct {
	State     string    `json:"state"`
	AuthURL   string    `json:"authUrl"`
	Realm     string    `json:"realm"`
	CreatedAt time.Time `json:"createdAt"`
}

// Bundle 授权完成后的凭证与账号信息。
type Bundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
	Realm        string `json:"realm"`
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func newClient() *http.Client {
	// 每次调用独立 cookie jar：多账号/多会话登录互不串。
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: Timeout, Jar: jar}
}

// applyHeaders 通用请求头（Origin/Referer 与 UA 按 realm 档案解析）。
func applyHeaders(req *http.Request, p realm.Profile) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", p.Origin)
	req.Header.Set("Referer", p.Origin+"/")
	req.Header.Set("User-Agent", p.ClientInfo(p.Fingerprint, "").UserAgent)
}

// doJSON 发请求并解 {code,msg,data} 信封；code != 0 返回错误（附带上游 msg）。
func doJSON(client *http.Client, method, fullURL string, p realm.Profile, authToken string, body []byte) (json.RawMessage, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, rd)
	if err != nil {
		return nil, 0, err
	}
	applyHeaders(req, p)
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// Start 发起授权：拿 state 与浏览器授权链接（用内置客户端）。
func Start(p realm.Profile) (*Session, error) { return StartWith(p, nil) }

// StartWith 同 Start，但可注入 HTTP 客户端（控制台复用网关客户端：统一超时/代理/连接池；
// 测试注入假 transport）。
func StartWith(p realm.Profile, base *http.Client) (*Session, error) {
	client := base
	if client == nil {
		client = newClient()
	}
	url := p.ChatBase + "/v2/plugin/auth/state?platform=" + p.Platform
	data, _, err := doJSON(client, http.MethodPost, url, p, "", []byte("{}"))
	if err != nil {
		return nil, fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, errors.New("auth state: missing state or authUrl")
	}
	return &Session{State: st.State, AuthURL: st.AuthURL, Realm: p.Name, CreatedAt: time.Now()}, nil
}

// Poll 查询授权结果：未完成返回 ErrPending；完成返回凭证 + 账号信息（用内置客户端）。
func Poll(p realm.Profile, state string) (*Bundle, error) { return PollWith(p, state, nil) }

// PollWith 同 Poll，但可注入 HTTP 客户端（见 StartWith）。
func PollWith(p realm.Profile, state string, base *http.Client) (*Bundle, error) {
	if state == "" {
		return nil, errors.New("empty state")
	}
	client := base
	if client == nil {
		client = newClient()
	}
	data, status, err := doJSON(client, http.MethodGet,
		p.ChatBase+"/v2/plugin/auth/token?state="+state, p, "", nil)
	if err != nil {
		// 未完成时上游返回业务 code 非 0（如 11217 "login ing"）；
		// 只有传输层/5xx 才是真错误，其余一律视为 pending（CLI 原行为）。
		if status == 0 || status >= 500 {
			return nil, fmt.Errorf("token endpoint error: %w", err)
		}
		return nil, ErrPending
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}
	b := &Bundle{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
		Realm:        p.Name,
	}
	// 账号信息（uid/nickname/enterpriseId）：失败不阻塞登录（凭证已拿到）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet,
		p.ChatBase+"/v2/plugin/login/account?state="+state, p, tok.AccessToken, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	b.UID, b.EnterpriseID, b.Nickname = acct.UID, acct.EnterpriseID, acct.Nickname
	return b, nil
}
