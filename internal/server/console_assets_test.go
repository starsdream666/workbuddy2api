package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleEmbeddedAssets(t *testing.T) {
	h, _ := consoleHandler(t, t.TempDir(), consoleUpstream("ok", nil), nil, nil)
	for _, tc := range []struct{ path, mime, marker string }{
		{"/admin", "text/html", "/admin/assets/console.js"},
		{"/admin/assets/console.css", "text/css", ".sidebar"},
		{"/admin/assets/theme.js", "text/javascript", "workbuddy.console.theme"},
		{"/admin/assets/metrics.js", "text/javascript", "filterEntries"},
		{"/admin/assets/console.js", "text/javascript", "initializeConsole"},
		{"/admin/assets/tasks.js", "text/javascript", "loadTasks"},
		{"/admin/assets/models.js", "text/javascript", "admin/api/models"},
		{"/admin/assets/packages.js", "text/javascript", "admin/api/account-packages"},
		{"/admin/assets/access.js", "text/javascript", "admin/api/auth/session"},
		{"/admin/assets/settings.js", "text/javascript", "admin/api/settings"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d", rr.Code)
			}
			if !strings.HasPrefix(rr.Header().Get("Content-Type"), tc.mime) {
				t.Fatalf("content type=%q", rr.Header().Get("Content-Type"))
			}
			if !strings.Contains(rr.Body.String(), tc.marker) {
				t.Fatalf("missing %q", tc.marker)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("must not serve stale embedded assets")
			}
		})
	}
}
