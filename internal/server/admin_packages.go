package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/upstream"
)

func (h *Handler) adminAccountPackages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	uid, requestedRealm := strings.TrimSpace(r.URL.Query().Get("uid")), strings.TrimSpace(r.URL.Query().Get("realm"))
	if uid == "" || len(uid) > 128 || requestedRealm == "" || !realm.Known(requestedRealm) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请提供有效的 realm 和 uid"})
		return
	}
	rn := realm.Normalize(requestedRealm)
	pl := h.pools()[rn]
	if len(h.pools()) == 0 && rn == realm.Normalize(h.cfg.DefaultRealm) {
		pl = h.cfg.Pool
	}
	if pl == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "该产品线未加载账号"})
		return
	}
	a := pl.AuthByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "凭证不存在，请刷新账号列表"})
		return
	}
	up := h.upstreamFor(rn)
	if up == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "上游服务未配置"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	details, err := up.AccountPackagesContext(ctx, a)
	if err != nil {
		status, message := http.StatusBadGateway, "套餐查询失败，上游服务暂不可用或返回了不完整数据，请稍后重试"
		var ue *upstream.Error
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			status, message = http.StatusGatewayTimeout, "套餐查询超时，请稍后重试"
		} else if errors.As(err, &ue) && (ue.Status == 401 || ue.Status == 403 || ue.Kind == upstream.ErrSessionDead) {
			message = "上游拒绝访问套餐信息，请检查账号授权状态或重新授权"
		}
		// Never expose raw upstream bodies: they may contain credentials or identity data.
		writeJSON(w, status, map[string]any{"error": message})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		UID   string `json:"uid"`
		Realm string `json:"realm"`
		*upstream.AccountPackages
	}{uid, rn, details})
}
