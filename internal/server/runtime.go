package server

import (
	"context"
	"time"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/taskqueue"
	"workbuddy2api/internal/upstream"
)

func (h *Handler) pools() map[string]*pool.Pool {
	if h.cfg.PoolsSnapshot != nil {
		return h.cfg.PoolsSnapshot()
	}
	return h.cfg.Pools
}

func (h *Handler) maintenance() map[string]func(context.Context, string, func(taskqueue.Progress)) error {
	if h.cfg.MaintenanceSnapshot != nil {
		return h.cfg.MaintenanceSnapshot()
	}
	return h.cfg.Maintenance
}

// RuntimeConfig is a per-request immutable view. Mutable pool/session/logger
// state remains on long-lived components, rather than being replaced on reload.
type RuntimeConfig struct {
	Upstream                        *upstream.Client
	Session                         *session.Router
	MaxBodyBytes                    int64
	SoftCooldown, CalibrateInterval time.Duration
	RefreshBalanceAfter             func(*pool.Pool, string) (int64, error)
}

func (h *Handler) CurrentRuntime() RuntimeConfig {
	if current := h.runtime.Load(); current != nil {
		return *current
	}
	return RuntimeConfig{Upstream: h.cfg.Upstream, Session: h.cfg.Session, MaxBodyBytes: h.cfg.MaxBodyBytes,
		SoftCooldown: h.cfg.SoftCooldown, CalibrateInterval: h.cfg.CalibrateInterval, RefreshBalanceAfter: h.cfg.RefreshBalanceAfter}
}

func (h *Handler) ApplyRuntime(next RuntimeConfig) { h.runtime.Store(&next) }

func (s *chatStat) runtimeFor(h *Handler) RuntimeConfig {
	if s.runtime != nil {
		return *s.runtime
	}
	return h.CurrentRuntime()
}
