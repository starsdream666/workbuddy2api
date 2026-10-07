package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/taskqueue"
)

func TestAdminTaskAuthorizationAndAliasDedup(test *testing.T) {
	queue := taskqueue.New(context.Background(), 4, 8)
	defer queue.Close()
	runner := func(ctx context.Context, kind string, report func(taskqueue.Progress)) error {
		<-ctx.Done()
		return ctx.Err()
	}
	handler := NewHandler(Config{ConsoleEnabled: true, APIKey: "master", RealmKeys: map[string]string{"cn": "cn-key", "codebuddy": "cb-key"}, Tasks: queue, Maintenance: map[string]func(context.Context, string, func(taskqueue.Progress)) error{"cn": runner, "workbuddy": runner}})
	request := func(method, path, key, body string) *httptest.ResponseRecorder {
		incoming := httptest.NewRequest(method, path, strings.NewReader(body))
		incoming.Header.Set("Authorization", "Bearer "+key)
		incoming.Header.Set("X-Realm", "workbuddy")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, incoming)
		return response
	}
	for _, scenario := range []struct {
		key, body string
		status    int
	}{
		{"bad", `{"realm":"cn","kind":"balance"}`, 401},
		{"cn-key", `{"realm":"workbuddy","kind":"balance"}`, 403},
		{"master", `{"realm":"unknown","kind":"balance"}`, 400},
		{"master", `{"realm":"cn","kind":"growth"}`, 400},
		{"master", `{"realm":"cn","kind":"balance","extra":true}`, 400},
		{"master", `{"realm":"cn","kind":"balance"}{}`, 400},
	} {
		if response := request(http.MethodPost, "/admin/api/tasks", scenario.key, scenario.body); response.Code != scenario.status {
			test.Fatalf("request=%s status=%d", scenario.body, response.Code)
		}
	}
	created := request(http.MethodPost, "/admin/api/tasks", "cb-key", `{"realm":"codebuddy","kind":"balance"}`)
	if created.Code != 202 {
		test.Fatal(created.Body.String())
	}
	var body struct {
		Task taskqueue.Task `json:"task"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &body); err != nil {
		test.Fatal(err)
	}
	if body.Task.Realm != "workbuddy" {
		test.Fatal("alias not canonicalized")
	}
	if response := request(http.MethodPost, "/admin/api/tasks", "master", `{"realm":"workbuddy","kind":"balance"}`); response.Code != 200 || !strings.Contains(response.Body.String(), `"deduplicated":true`) {
		test.Fatal("shared credentials not deduplicated")
	}
	if response := request(http.MethodGet, "/admin/api/tasks", "cn-key", ""); strings.Contains(response.Body.String(), "workbuddy") {
		test.Fatal("cross-realm task disclosure")
	}
	if response := request(http.MethodDelete, "/admin/api/tasks/"+body.Task.ID, "cn-key", ""); response.Code != 404 {
		test.Fatal("cross-realm cancel permitted")
	}
	if response := request(http.MethodDelete, "/admin/api/tasks/"+body.Task.ID, "cb-key", ""); response.Code != 200 {
		test.Fatal("owner cannot cancel")
	}
}
