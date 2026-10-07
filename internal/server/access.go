package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/access"
	"workbuddy2api/internal/realm"
)

const adminCookie = "wb2api_session"

func (h *Handler) keyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"service": ServiceName, "channels": h.realmsForCredential(r)})
}

type accessContextKey struct{}
type loginAttempts struct {
	mu      sync.Mutex
	windows map[string]loginWindow
	gate    chan struct{}
}
type loginWindow struct {
	until time.Time
	count int
}

func (h *Handler) allowLogin(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	h.logins.mu.Lock()
	defer h.logins.mu.Unlock()
	now := time.Now()
	for k, v := range h.logins.windows {
		if now.After(v.until) {
			delete(h.logins.windows, k)
		}
	}
	v, exists := h.logins.windows[ip]
	if !exists {
		if len(h.logins.windows) >= 1024 {
			return false
		}
		v.until = now.Add(5 * time.Minute)
	}
	if v.count >= 10 {
		return false
	}
	v.count++
	h.logins.windows[ip] = v
	return true
}
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return ""
	}
	return c.Value
}
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
	}
	return true
}
func readAdminJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		writeJSON(w, 415, map[string]any{"error": "请使用 application/json"})
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, 400, map[string]any{"error": "请求内容无效"})
		return false
	}
	if dec.Decode(new(any)) != io.EOF {
		writeJSON(w, 400, map[string]any{"error": "请求必须为单个 JSON 对象"})
		return false
	}
	return true
}
func (h *Handler) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if token == "" {
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: token, Path: "/admin", HttpOnly: true, Secure: h.cfg.SecureCookie || r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge})
}
func (h *Handler) registerAccess() {
	h.mux.HandleFunc("GET /admin/api/auth/session", h.adminSession)
	h.mux.HandleFunc("POST /admin/api/auth/login", h.adminPasswordLogin)
	h.mux.HandleFunc("POST /admin/api/auth/setup", h.adminSetup)
	h.mux.HandleFunc("POST /admin/api/auth/logout", h.withAdminAuth(h.adminLogout))
	h.mux.HandleFunc("POST /admin/api/auth/password", h.withAdminAuth(h.adminPasswordChange))
	h.mux.HandleFunc("GET /admin/api/keys", h.withAdminAuth(h.adminKeys))
	h.mux.HandleFunc("POST /admin/api/keys", h.withAdminAuth(h.adminCreateKey))
	h.mux.HandleFunc("PATCH /admin/api/keys/{id}", h.withAdminAuth(h.adminUpdateKey))
	h.mux.HandleFunc("DELETE /admin/api/keys/{id}", h.withAdminAuth(h.adminRevokeKey))
}
func (h *Handler) adminSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.cfg.Access.Session(sessionToken(r))
	body := map[string]any{"initialized": h.cfg.Access.Ready(), "authenticated": ok}
	if ok {
		body["username"] = sess.Username
		body["csrf_token"] = sess.CSRF
		body["expires_at"] = sess.ExpiresAt
	}
	writeJSON(w, 200, body)
}
func (h *Handler) loginRequest(w http.ResponseWriter, r *http.Request) bool {
	if !sameOrigin(r) {
		writeJSON(w, 403, map[string]any{"error": "不允许跨站登录请求"})
		return false
	}
	if !h.allowLogin(r) {
		w.Header().Set("Retry-After", "300")
		writeJSON(w, 429, map[string]any{"error": "尝试次数过多，请 5 分钟后重试"})
		return false
	}
	select {
	case h.logins.gate <- struct{}{}:
		return true
	default:
		writeJSON(w, 429, map[string]any{"error": "登录服务繁忙，请稍后重试"})
		return false
	}
}
func (h *Handler) adminPasswordLogin(w http.ResponseWriter, r *http.Request) {
	if !h.loginRequest(w, r) {
		return
	}
	defer func() { <-h.logins.gate }()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readAdminJSON(w, r, &body) {
		return
	}
	token, sess, err := h.cfg.Access.Login(body.Username, body.Password)
	if err != nil {
		writeJSON(w, 401, map[string]any{"error": "账号或密码错误"})
		return
	}
	// Re-login rotates the browser session instead of accumulating old cookies.
	h.cfg.Access.Logout(sessionToken(r))
	h.setSessionCookie(w, r, token, sess.ExpiresAt)
	writeJSON(w, 200, map[string]any{"authenticated": true, "username": sess.Username, "csrf_token": sess.CSRF, "expires_at": sess.ExpiresAt})
}
func (h *Handler) adminSetup(w http.ResponseWriter, r *http.Request) {
	if !h.loginRequest(w, r) {
		return
	}
	defer func() { <-h.logins.gate }()
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Token    string `json:"setup_token"`
	}
	if !readAdminJSON(w, r, &body) {
		return
	}
	if err := h.cfg.Access.Setup(body.Token, body.Username, body.Password); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (h *Handler) adminLogout(w http.ResponseWriter, r *http.Request) {
	h.cfg.Access.Logout(sessionToken(r))
	h.setSessionCookie(w, r, "", time.Unix(1, 0))
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (h *Handler) adminPasswordChange(w http.ResponseWriter, r *http.Request) {
	if !h.loginRequest(w, r) {
		return
	}
	defer func() { <-h.logins.gate }()
	var body struct {
		Current string `json:"current_password"`
		Next    string `json:"new_password"`
	}
	if !readAdminJSON(w, r, &body) {
		return
	}
	if err := h.cfg.Access.ChangePassword(body.Current, body.Next); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	h.setSessionCookie(w, r, "", time.Unix(1, 0))
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (h *Handler) adminKeys(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"keys": h.cfg.Access.List(), "channels": realm.All(), "active_channels": h.activeRealms()})
}
func (h *Handler) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var p access.Policy
	if !readAdminJSON(w, r, &p) {
		return
	}
	key, token, err := h.cfg.Access.Create(p)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]any{"key": key, "token": token})
}
func (h *Handler) adminUpdateKey(w http.ResponseWriter, r *http.Request) {
	var p access.Policy
	if !readAdminJSON(w, r, &p) {
		return
	}
	key, err := h.cfg.Access.Update(r.PathValue("id"), p)
	if err != nil {
		status := 400
		if errors.Is(err, access.ErrNotFound) {
			status = 404
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"key": key})
}
func (h *Handler) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.Access.Revoke(r.PathValue("id")); err != nil {
		status := 500
		if errors.Is(err, access.ErrNotFound) {
			status = 404
		}
		writeJSON(w, status, map[string]any{"error": "吊销失败，请确认密钥存在且认证数据可写"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (h *Handler) withAdminSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := h.cfg.Access.Session(sessionToken(r))
		if !ok {
			writeJSON(w, 401, map[string]any{"error": "请先登录管理账号"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && (!sameOrigin(r) || !secretEqual(r.Header.Get("X-CSRF-Token"), sess.CSRF)) {
			writeJSON(w, 403, map[string]any{"error": "登录会话校验失败，请刷新页面"})
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), sess.ExpiresAt)
		defer cancel()
		go func() {
			select {
			case <-sess.Done:
				cancel()
			case <-ctx.Done():
			}
		}()
		next(w, r.WithContext(ctx))
	}
}

func (h *Handler) withIssuedKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := h.cfg.Access.Admit(bearerToken(r))
		if err != nil {
			status := 401
			kind := "invalid_api_key"
			if errors.Is(err, access.ErrRateLimit) {
				status = 429
				kind = "rate_limit_exceeded"
				w.Header().Set("Retry-After", "60")
			}
			writeOpenAIError(w, status, kind, err.Error())
			return
		}
		if selected := strings.TrimSpace(r.Header.Get("X-Realm")); selected != "" && (!realm.Known(selected) || !key.Allows(selected)) {
			writeOpenAIError(w, 403, "channel_not_allowed", "该密钥不允许访问指定渠道")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), accessContextKey{}, key)))
	}
}
func (h *Handler) issuedChannelAllowed(r *http.Request, channel string) bool {
	key, ok := r.Context().Value(accessContextKey{}).(access.Key)
	if !ok || !key.Allows(channel) {
		return false
	}
	for _, rn := range h.activeRealms() {
		if rn == channel {
			return true
		}
	}
	return false
}
