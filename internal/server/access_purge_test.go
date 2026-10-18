package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/access"
)

// TestPurgeKeyRemovesRecordEntirely 硬删除与「吊销」必须分得开：
//   - DELETE /admin/api/keys/{id}        软删（置 revoked_at），记录留在列表里；
//   - DELETE /admin/api/keys/{id}/purge  硬删，记录从列表里彻底消失。
//
// 同时钉住那条边界：老调用方重试 DELETE 只会吊销、不会删记录 —— 两个动作走的是
// 不同路径，所以「手滑重试」不会把密钥抹掉。
func TestPurgeKeyRemovesRecordEntirely(t *testing.T) {
	h, st := managedHandler(t)
	cookie, csrf := adminSessionForTest(t, h, st)
	policy := `{"name":"purge-me","channels":["workbuddy"],"default_channel":"workbuddy","rpm":0,"enabled":true,"expires_at":null}`
	w := accessRequest(h, "POST", "/admin/api/keys", policy, "", cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var created struct {
		Key access.Key `json:"key"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	// 吊销 = 软删：记录仍在列表里。
	if w = accessRequest(h, "DELETE", "/admin/api/keys/"+created.Key.ID, "", "", cookie, csrf); w.Code != 200 {
		t.Fatalf("revoke %d %s", w.Code, w.Body)
	}
	if w = accessRequest(h, "GET", "/admin/api/keys", "", "", cookie, ""); w.Code != 200 || !strings.Contains(w.Body.String(), created.Key.ID) {
		t.Fatalf("revoked key should stay in list: %d %s", w.Code, w.Body)
	}

	// 硬删 = 记录消失。
	if w = accessRequest(h, "DELETE", "/admin/api/keys/"+created.Key.ID+"/purge", "", "", cookie, csrf); w.Code != 200 {
		t.Fatalf("purge %d %s", w.Code, w.Body)
	}
	w = accessRequest(h, "GET", "/admin/api/keys", "", "", cookie, "")
	if strings.Contains(w.Body.String(), created.Key.ID) {
		t.Errorf("purged key still listed: %s", w.Body)
	}

	// 重复删除返回 404（既不是 500，也不静默成功）。
	if w = accessRequest(h, "DELETE", "/admin/api/keys/"+created.Key.ID+"/purge", "", "", cookie, csrf); w.Code != 404 {
		t.Errorf("second purge=%d want 404", w.Code)
	}
}

// TestPurgeKeyRequiresCSRF 硬删不可撤销，必须过 CSRF 校验；被拒时密钥毫发无损。
func TestPurgeKeyRequiresCSRF(t *testing.T) {
	h, st := managedHandler(t)
	cookie, csrf := adminSessionForTest(t, h, st)
	policy := `{"name":"csrf-me","channels":["workbuddy"],"default_channel":"workbuddy","rpm":0,"enabled":true,"expires_at":null}`
	w := accessRequest(h, "POST", "/admin/api/keys", policy, "", cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	var created struct {
		Key access.Key `json:"key"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	if w = accessRequest(h, "DELETE", "/admin/api/keys/"+created.Key.ID+"/purge", "", "", cookie, ""); w.Code != 403 {
		t.Errorf("purge without CSRF=%d want 403", w.Code)
	}
	if w = accessRequest(h, "GET", "/admin/api/keys", "", "", cookie, ""); !strings.Contains(w.Body.String(), created.Key.ID) {
		t.Error("key must survive a CSRF-rejected purge")
	}
}
