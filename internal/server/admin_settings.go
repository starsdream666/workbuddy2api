package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"workbuddy2api/internal/settings"
)

func settingsError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var invalid *settings.Invalid
	if errors.As(err, &invalid) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, settings.ErrConflict) {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
func (h *Handler) adminSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, 503, map[string]any{"error": "当前实例未提供服务设置"})
		return
	}
	snapshot, err := h.cfg.Settings.Read()
	if err != nil {
		settingsError(w, err)
		return
	}
	writeJSON(w, 200, snapshot)
}
func (h *Handler) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeJSON(w, 503, map[string]any{"error": "当前实例未提供服务设置"})
		return
	}
	var body struct {
		Revision string                     `json:"revision"`
		Changes  map[string]json.RawMessage `json:"changes"`
	}
	if !readAdminJSON(w, r, &body) {
		return
	}
	snapshot, err := h.cfg.Settings.Save(body.Revision, body.Changes)
	if err != nil {
		settingsError(w, err)
		return
	}
	writeJSON(w, 200, snapshot)
}
