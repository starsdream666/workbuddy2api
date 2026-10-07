package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"workbuddy2api/internal/realm"
	"workbuddy2api/internal/taskqueue"
)

func (handler *Handler) taskRealmAllowed(request *http.Request, name string) bool {
	if handler.maintenance()[name] == nil {
		return false
	}
	if handler.cfg.Access != nil {
		return true
	} // Admin session already verified by middleware.
	token := bearerToken(request)
	if handler.cfg.APIKey != "" && secretEqual(token, handler.cfg.APIKey) {
		return true
	}
	if handler.cfg.APIKey == "" && len(handler.cfg.RealmKeys) == 0 {
		return true
	}
	for route, key := range handler.cfg.RealmKeys {
		if key != "" && secretEqual(token, key) && realm.AuthRealmOf(route) == name {
			return true
		}
	}
	return false
}

func (handler *Handler) adminTasks(writer http.ResponseWriter, request *http.Request) {
	tasks := make([]taskqueue.Task, 0)
	realms := make([]string, 0)
	for name := range handler.maintenance() {
		if handler.taskRealmAllowed(request, name) {
			realms = append(realms, name)
		}
	}
	sort.Strings(realms)
	if handler.cfg.Tasks != nil {
		for _, task := range handler.cfg.Tasks.List() {
			if handler.taskRealmAllowed(request, task.Realm) {
				tasks = append(tasks, task)
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"enabled": handler.cfg.Tasks != nil, "realms": realms, "tasks": tasks})
}

func (handler *Handler) adminTaskCreate(writer http.ResponseWriter, request *http.Request) {
	if handler.cfg.Tasks == nil {
		writeOpenAIError(writer, http.StatusNotImplemented, "unavailable", "task queue is unavailable")
		return
	}
	var body struct {
		Realm string `json:"realm"`
		Kind  string `json:"kind"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request", "invalid task request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request", "expected one JSON object")
		return
	}
	if !realm.Known(body.Realm) || (body.Kind != "balance" && body.Kind != "refresh_tokens") {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request", "unknown realm or maintenance operation")
		return
	}
	name := realm.AuthRealmOf(body.Realm)
	if !handler.taskRealmAllowed(request, name) {
		writeOpenAIError(writer, http.StatusForbidden, "forbidden", "no maintenance access to this credential realm")
		return
	}
	runner := handler.maintenance()[name]
	task, duplicate, err := handler.cfg.Tasks.Enqueue(name, body.Kind, func(ctx context.Context, report func(taskqueue.Progress)) error {
		return runner(ctx, body.Kind, report)
	})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, taskqueue.ErrFull) {
			status = http.StatusTooManyRequests
		}
		writeOpenAIError(writer, status, "queue_unavailable", err.Error())
		return
	}
	status := http.StatusAccepted
	if duplicate {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{"task": task, "deduplicated": duplicate})
}

func (handler *Handler) adminTaskCancel(writer http.ResponseWriter, request *http.Request) {
	if handler.cfg.Tasks != nil {
		for _, task := range handler.cfg.Tasks.List() {
			if task.ID != request.PathValue("id") || !handler.taskRealmAllowed(request, task.Realm) {
				continue
			}
			if updated, found := handler.cfg.Tasks.Cancel(task.ID); found {
				writeJSON(writer, http.StatusOK, updated)
				return
			}
		}
	}
	writeOpenAIError(writer, http.StatusNotFound, "not_found", "task not found")
}
