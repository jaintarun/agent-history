package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tarunjain/agent-history/internal/analyze"
	"github.com/tarunjain/agent-history/internal/launch"
	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
)

func TestSecurityMiddleware(t *testing.T) {
	handler, _, _, _, _, _ := testHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	request.Host = "127.0.0.1:1234"
	if response := serve(handler, request); response.Code != http.StatusNotFound {
		t.Fatalf("missing token status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/test-token/api/health", nil)
	request.Host = "example.com"
	if response := serve(handler, request); response.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host status = %d", response.Code)
	}

	request = jsonRequest(http.MethodPost, "/test-token/api/scan", `{}`)
	request.Header.Set("Origin", "https://evil.example")
	if response := serve(handler, request); response.Code != http.StatusForbidden {
		t.Fatalf("invalid Origin status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/test-token/api/health", nil)
	request.Host = "127.0.0.1:1234"
	if response := serve(handler, request); response.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestPlainURLHandlerServesRootPaths(t *testing.T) {
	_, database, scanner, queue, launcher, _ := testHandler(t)
	handler, err := NewHandler(Config{
		PlainURL: true, Store: database, Scanner: scanner, Queue: queue,
		Launcher: launcher, AnalysisDefaults: analyze.Options{
			Provider: "codex-cli", Model: "test", PromptVersion: "v1", NormalizerVersion: "v1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response := serve(handler, apiRequest(http.MethodGet, "/api/health", nil)); response.Code != http.StatusOK {
		t.Fatalf("root health status = %d body=%s", response.Code, response.Body.String())
	}
	if response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/health", nil)); response.Code != http.StatusNotFound {
		t.Fatalf("token-prefixed health status = %d, want 404", response.Code)
	}
}

func TestEmbeddedWebApplication(t *testing.T) {
	handler, _, _, _, _, _ := testHandler(t)
	tests := []struct {
		path        string
		contentType string
		contains    string
		cache       string
	}{
		{path: "/test-token/", contentType: "text/html", contains: `assets/app.js?v=3`, cache: "no-store"},
		{path: "/test-token/assets/app.css?v=3", contentType: "text/css", contains: ":root", cache: "no-store"},
		{path: "/test-token/assets/app.js?v=3", contentType: "text/javascript", contains: "fetch(", cache: "no-store"},
	}
	for _, test := range tests {
		response := serve(handler, apiRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d body=%s", test.path, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); !strings.Contains(got, test.contentType) {
			t.Errorf("GET %s Content-Type = %q", test.path, got)
		}
		if !strings.Contains(response.Body.String(), test.contains) {
			t.Errorf("GET %s body missing %q", test.path, test.contains)
		}
		if response.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("GET %s missing Content-Security-Policy", test.path)
		}
		if got := response.Header().Get("Cache-Control"); got != test.cache {
			t.Errorf("GET %s Cache-Control = %q, want %q", test.path, got, test.cache)
		}
		if strings.Contains(response.Body.String(), "https://") || strings.Contains(response.Body.String(), "http://") {
			t.Errorf("GET %s contains external asset reference", test.path)
		}
	}
}

func TestWebApplicationIncludesCoreWorkflows(t *testing.T) {
	html, err := webAssets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	markup := string(html)
	for _, id := range []string{
		"session-search", "agent-filter", "active-filter", "cmux-filter", "cwd-filter", "topic-filter",
		"status-filter", "sort-filter", "active-after-filter", "active-before-filter",
		"started-after-filter", "started-before-filter", "session-results", "detail-content",
		"confirm-dialog", "command-dialog", "settings-dialog", "retitle-weak-button", "refresh-button",
		"refresh-label", "refresh-countdown", "auto-refresh-toggle", "settings-cmux-sync", "settings-cmux-status",
	} {
		if !strings.Contains(markup, `id="`+id+`"`) {
			t.Errorf("embedded HTML missing control %q", id)
		}
	}
	if !strings.Contains(markup, "Automatically sync titles to cmux") {
		t.Error("embedded HTML missing cmux automatic sync label")
	}
	javascript, err := webAssets.ReadFile("assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	script := string(javascript)
	for _, workflow := range []string{"/analyze", "/analysis", "/rescan", "/launch", "/scan", "/settings", "/cmux/refresh", "/cmux-title"} {
		if !strings.Contains(script, workflow) {
			t.Errorf("embedded JavaScript missing workflow %q", workflow)
		}
	}
	for _, behavior := range []string{
		"const refreshIntervalSeconds = 60;",
		`"cmux-filter": "cmux"`,
		`["cmux-filter", "cmux"]`,
		`element("span", ` + "`cmux-label ${session.cmux.lifecycle}`" + `, `,
		`function renderCmuxComparison(session)`,
		`Different - cmux title preserved`,
		"function startRefreshCountdown()",
		"async function refreshPageData(showCmuxError)",
		"await loadSessions(false);",
		`elements["refresh-countdown"].textContent = `,
		`void refreshPageData(false);`,
		`elements["refresh-button"].addEventListener("click", () => refreshPageData(true));`,
	} {
		if !strings.Contains(script, behavior) {
			t.Errorf("embedded JavaScript missing refresh behavior %q", behavior)
		}
	}
	for _, behavior := range []string{
		`const autoRefreshStorageKey = "agent-history.auto-refresh";`,
		`function loadAutoRefreshPreference()`,
		`function saveAutoRefreshPreference(enabled)`,
		`function stopScheduledRefreshes()`,
		`function scheduleSelectedSessionPoll()`,
		`if (!state.autoRefresh)`,
		`state.selectedAnalysisStatus = session.analysis_status;`,
		`elements["auto-refresh-toggle"].addEventListener("change"`,
	} {
		if !strings.Contains(script, behavior) {
			t.Errorf("embedded JavaScript missing auto-refresh behavior %q", behavior)
		}
	}
	css, err := webAssets.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	styles := string(css)
	for _, style := range []string{
		"#refresh-button { width: 126px;",
		".refresh-countdown { color: var(--text-muted);",
		".auto-refresh-control {",
		"width: 104px;",
		"white-space: nowrap;",
		".cmux-label {",
		".cmux-band {",
		"font-variant-numeric: tabular-nums;",
		"@media (max-width: 480px)",
		".header-actions { justify-content: flex-start; gap: 6px; }",
	} {
		if !strings.Contains(styles, style) {
			t.Errorf("embedded CSS missing refresh style %q", style)
		}
	}
}

func TestWebApplicationHeaderStacksAtIntermediateWidths(t *testing.T) {
	css, err := webAssets.ReadFile("assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	styles := string(css)
	start := strings.Index(styles, "@media (max-width: 800px)")
	if start < 0 {
		t.Fatal("embedded CSS missing 800px breakpoint")
	}
	remainder := styles[start:]
	end := strings.Index(remainder[1:], "@media ")
	if end < 0 {
		t.Fatal("embedded CSS missing breakpoint after 800px contract")
	}
	intermediate := remainder[:end+1]
	for _, style := range []string{
		".app-header { position: sticky; top: 0; z-index: 30; display: block; }",
		".brand-block { margin-bottom: 8px; padding-top: 0; }",
		".header-actions { justify-content: flex-start; }",
	} {
		if !strings.Contains(intermediate, style) {
			t.Errorf("800px header contract missing %q", style)
		}
	}
	for _, style := range []string{
		"#refresh-button { width: 126px;",
		".auto-refresh-control { width: 104px;",
		"@media (max-width: 480px)",
		".header-actions button { padding: 0 8px; }",
	} {
		if !strings.Contains(styles, style) {
			t.Errorf("responsive refresh contract missing %q", style)
		}
	}
}

func TestSessionReadEndpoints(t *testing.T) {
	handler, _, _, _, _, _ := testHandler(t)

	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions?q=login&agent=codex", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("search status = %d body=%s", response.Code, response.Body.String())
	}
	var search struct {
		Sessions []struct {
			ID    string `json:"id"`
			Agent string `json:"agent"`
		} `json:"sessions"`
	}
	decodeResponse(t, response, &search)
	if len(search.Sessions) != 1 || search.Sessions[0].ID != "session-1" || search.Sessions[0].Agent != "codex" {
		t.Fatalf("search response = %#v", search)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status = %d body=%s", response.Code, response.Body.String())
	}
	var detail map[string]any
	decodeResponse(t, response, &detail)
	if detail["title"] != "Login retry investigation" || detail["span_seconds"].(float64) != 3600 {
		t.Fatalf("detail response = %#v", detail)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1/messages?include_tools=false", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("messages status = %d body=%s", response.Code, response.Body.String())
	}
	var messages struct {
		Messages []map[string]any `json:"messages"`
	}
	decodeResponse(t, response, &messages)
	if len(messages.Messages) != 2 || messages.Messages[0]["role"] != "user" {
		t.Fatalf("messages response = %#v", messages)
	}

	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/facets", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("facets status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestCmuxSessionStateAndFiltering(t *testing.T) {
	application, database, _, _, _, _ := testHandler(t)
	observedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if err := database.ReplaceCmuxSnapshot(context.Background(), store.CmuxStatus{
		Available: true, AccessMode: "allowAll", ObservedAt: observedAt,
	}, []store.CmuxSessionState{{
		SessionID: "session-1", Open: true, WorkspaceID: "workspace-1", SurfaceID: "surface-1",
		WorkspaceTitle: "Old cmux title", SurfaceTitle: "Codex", Lifecycle: "running", ObservedAt: observedAt,
	}}); err != nil {
		t.Fatal(err)
	}
	integration := &fakeCmux{status: store.CmuxStatus{Available: true, AccessMode: "allowAll", ObservedAt: observedAt}}
	application.(*handler).cmux = integration

	response := serve(application, apiRequest(http.MethodGet, "/test-token/api/sessions?cmux=open", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("open filter status = %d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Sessions []struct {
			ID   string `json:"id"`
			Cmux *struct {
				Open           bool   `json:"open"`
				Lifecycle      string `json:"lifecycle"`
				WorkspaceTitle string `json:"workspace_title"`
				SurfaceTitle   string `json:"surface_title"`
				Target         string `json:"target"`
				TargetTitle    string `json:"target_title"`
				TitleState     string `json:"title_state"`
				ObservedAt     string `json:"observed_at"`
			} `json:"cmux"`
		} `json:"sessions"`
	}
	decodeResponse(t, response, &result)
	if len(result.Sessions) != 1 || result.Sessions[0].ID != "session-1" || result.Sessions[0].Cmux == nil {
		t.Fatalf("open filter response = %#v", result)
	}
	cmux := result.Sessions[0].Cmux
	if !cmux.Open || cmux.Lifecycle != "running" || cmux.WorkspaceTitle != "Old cmux title" ||
		cmux.SurfaceTitle != "Codex" || cmux.Target != "workspace" || cmux.TargetTitle != "Old cmux title" ||
		cmux.TitleState != "different" || cmux.ObservedAt != observedAt.Format(time.RFC3339Nano) {
		t.Fatalf("cmux response = %#v", cmux)
	}

	response = serve(application, apiRequest(http.MethodGet, "/test-token/api/sessions?cmux=closed", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("closed filter status = %d body=%s", response.Code, response.Body.String())
	}
	decodeResponse(t, response, &result)
	if len(result.Sessions) != 0 {
		t.Fatalf("closed filter response = %#v", result)
	}
	response = serve(application, apiRequest(http.MethodGet, "/test-token/api/sessions?cmux=invalid", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid cmux filter status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestCmuxRefreshAndManualTitlePush(t *testing.T) {
	application, _, _, _, _, _ := testHandler(t)
	integration := &fakeCmux{status: store.CmuxStatus{Available: true, AccessMode: "allowAll"}}
	application.(*handler).cmux = integration

	response := serve(application, jsonRequest(http.MethodPost, "/test-token/api/cmux/refresh", `{}`))
	if response.Code != http.StatusOK || integration.refreshCalls != 1 {
		t.Fatalf("refresh response=%d calls=%d body=%s", response.Code, integration.refreshCalls, response.Body.String())
	}
	response = serve(application, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/cmux-title", `{}`))
	if response.Code != http.StatusOK || len(integration.pushIDs) != 1 || integration.pushIDs[0] != "session-1" {
		t.Fatalf("push response=%d ids=%v body=%s", response.Code, integration.pushIDs, response.Body.String())
	}
	response = serve(application, jsonRequest(http.MethodPost, "/test-token/api/sessions/missing/cmux-title", `{}`))
	if response.Code != http.StatusNotFound || len(integration.pushIDs) != 1 {
		t.Fatalf("missing push response=%d ids=%v body=%s", response.Code, integration.pushIDs, response.Body.String())
	}

	integration.err = errors.New("dial unix /private/cmux.sock: connection refused")
	response = serve(application, jsonRequest(http.MethodPost, "/test-token/api/cmux/refresh", `{}`))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("partial reconciliation status = %d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"code":"cmux_reconciliation_failed"`) ||
		!strings.Contains(body, `"message":"cmux reconciliation could not be completed"`) {
		t.Fatalf("partial reconciliation body = %s", body)
	} else if strings.Contains(body, "/private/cmux.sock") {
		t.Fatalf("partial reconciliation response leaked socket path: %s", body)
	}

	integration.status = store.CmuxStatus{Available: false, Error: "cmux is unavailable"}
	response = serve(application, jsonRequest(http.MethodPost, "/test-token/api/cmux/refresh", `{}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable refresh status = %d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "/private/cmux.sock") {
		t.Fatalf("unavailable response leaked socket path: %s", response.Body.String())
	}
}

func TestCmuxSettingsPersistAutomaticSyncAndExposeStatus(t *testing.T) {
	application, database, _, _, _, _ := testHandler(t)
	observedAt := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	integration := &fakeCmux{status: store.CmuxStatus{
		Available: true, AccessMode: "allowAll", ObservedAt: observedAt,
	}}
	application.(*handler).cmux = integration

	response := serve(application, apiRequest(http.MethodGet, "/test-token/api/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get settings status = %d body=%s", response.Code, response.Body.String())
	}
	var settings struct {
		CmuxTitleSync    bool   `json:"cmux_title_sync"`
		CmuxAvailable    bool   `json:"cmux_available"`
		CmuxAccessMode   string `json:"cmux_access_mode"`
		CmuxError        string `json:"cmux_error"`
		CmuxObservedAt   string `json:"cmux_observed_at"`
		AnalysisProvider string `json:"analysis_provider"`
	}
	decodeResponse(t, response, &settings)
	if settings.CmuxTitleSync || !settings.CmuxAvailable || settings.CmuxAccessMode != "allowAll" ||
		settings.CmuxError != "" || settings.CmuxObservedAt != observedAt.Format(time.RFC3339Nano) ||
		settings.AnalysisProvider != "codex-cli" {
		t.Fatalf("cmux settings = %#v", settings)
	}

	response = serve(application, jsonRequest(http.MethodPut, "/test-token/api/settings", `{"cmux_title_sync":true}`))
	if response.Code != http.StatusOK {
		t.Fatalf("put cmux settings status = %d body=%s", response.Code, response.Body.String())
	}
	value, ok, err := database.Setting(context.Background(), "cmux.title_sync")
	if err != nil || !ok || value != "true" {
		t.Fatalf("stored cmux title sync = %q, %v, %v", value, ok, err)
	}
}

func TestMutationEndpoints(t *testing.T) {
	handler, database, scanner, queue, launcher, _ := testHandler(t)

	response := serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{"model":"override-model","full":true}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("analyze status = %d body=%s", response.Code, response.Body.String())
	}
	queue.mu.Lock()
	if len(queue.jobs) != 1 || queue.jobs[0].sessionID != "session-1" || queue.jobs[0].options.Model != "override-model" || !queue.jobs[0].options.Full {
		t.Fatalf("queued jobs = %#v", queue.jobs)
	}
	queue.mu.Unlock()

	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/retitle", `{}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("retitle status = %d body=%s", response.Code, response.Body.String())
	}
	queue.mu.Lock()
	if len(queue.jobs) != 2 || queue.jobs[1].kind != "retitle" || queue.jobs[1].sessionID != "session-1" {
		t.Fatalf("retitle jobs = %#v", queue.jobs)
	}
	queue.mu.Unlock()

	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/retitle-weak", `{}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("bulk retitle status = %d body=%s", response.Code, response.Body.String())
	}
	var bulk map[string]int
	decodeResponse(t, response, &bulk)
	if bulk["matched"] != 1 || bulk["queued"] != 1 {
		t.Fatalf("bulk retitle response = %#v", bulk)
	}

	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{"agent":"codex"}`))
	if response.Code != http.StatusOK || scanner.scanAgent != "codex" {
		t.Fatalf("scan response=%d scanner=%#v body=%s", response.Code, scanner, response.Body.String())
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/rescan", `{}`))
	if response.Code != http.StatusOK || scanner.rescanID != "session-1" {
		t.Fatalf("rescan response=%d scanner=%#v", response.Code, scanner)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/launch", `{"launcher":"copy"}`))
	if response.Code != http.StatusOK ||
		launcher.sessionID != "session-1" ||
		launcher.mode != "copy" ||
		launcher.permissions != launch.PermissionNormal {
		t.Fatalf("launch response=%d launcher=%#v body=%s", response.Code, launcher, response.Body.String())
	}

	request := apiRequest(http.MethodDelete, "/test-token/api/sessions/session-1/analysis", nil)
	request.Header.Set("Origin", "http://127.0.0.1:1234")
	response = serve(handler, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete analysis status = %d body=%s", response.Code, response.Body.String())
	}
	detail, err := database.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Session.Title != "" || len(detail.Messages) != 2 {
		t.Fatalf("detail after deletion = %#v", detail)
	}
}

func TestLaunchPermissionModeIsValidated(t *testing.T) {
	handler, _, _, _, launcher, _ := testHandler(t)

	response := serve(handler, jsonRequest(
		http.MethodPost,
		"/test-token/api/sessions/session-1/launch",
		`{"launcher":"auto","permissions":"bypass"}`,
	))
	if response.Code != http.StatusOK ||
		launcher.permissions != launch.PermissionBypass ||
		launcher.calls != 1 {
		t.Fatalf("bypass response=%d launcher=%#v body=%s", response.Code, launcher, response.Body.String())
	}

	response = serve(handler, jsonRequest(
		http.MethodPost,
		"/test-token/api/sessions/session-1/launch",
		`{"launcher":"auto","permissions":"--arbitrary"}`,
	))
	if response.Code != http.StatusBadRequest || launcher.calls != 1 {
		t.Fatalf("invalid response=%d launcher=%#v body=%s", response.Code, launcher, response.Body.String())
	}
}

func TestSettingsAreValidatedAndNeverExposeSecrets(t *testing.T) {
	handler, database, _, queue, _, _ := testHandler(t)
	if err := database.SetSetting(context.Background(), "analysis.model", "initial-model"); err != nil {
		t.Fatal(err)
	}

	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get settings status = %d", response.Code)
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "key") || strings.Contains(strings.ToLower(response.Body.String()), "credential") {
		t.Fatalf("settings response appears to expose secret fields: %s", response.Body.String())
	}

	response = serve(handler, jsonRequest(http.MethodPut, "/test-token/api/settings", `{"analysis_provider":"codex-cli","analysis_model":"new-model","analysis_auto":false}`))
	if response.Code != http.StatusOK {
		t.Fatalf("put settings status = %d body=%s", response.Code, response.Body.String())
	}
	model, ok, err := database.Setting(context.Background(), "analysis.model")
	if err != nil || !ok || model != "new-model" {
		t.Fatalf("stored model = %q, %v, %v", model, ok, err)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("analyze status = %d body=%s", response.Code, response.Body.String())
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.jobs) != 1 || queue.jobs[0].options.Model != "new-model" {
		t.Fatalf("analysis did not use saved model: %#v", queue.jobs)
	}
}

func TestJSONValidationErrorsAndNotFound(t *testing.T) {
	handler, _, _, queue, _, _ := testHandler(t)
	response := serve(handler, apiRequest(http.MethodPost, "/test-token/api/scan", strings.NewReader(`{}`)))
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status = %d", response.Code)
	}
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{broken`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d", response.Code)
	}
	response = serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d", response.Code)
	}
	queue.err = errors.New("queue unavailable")
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/sessions/session-1/analyze", `{}`))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("queue error status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestAccessLogsDoNotContainTranscriptText(t *testing.T) {
	handler, _, _, _, _, logs := testHandler(t)
	response := serve(handler, apiRequest(http.MethodGet, "/test-token/api/sessions/session-1/messages", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if strings.Contains(logs.String(), "sensitive transcript phrase") {
		t.Fatalf("access log contains transcript content: %s", logs.String())
	}
}

func TestMiddlewareRejectsOversizedBodiesAndRecoversPanics(t *testing.T) {
	handler, _, scanner, _, _, _ := testHandler(t)
	large := `{"agent":"` + strings.Repeat("x", maxJSONBody) + `"}`
	response := serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", large))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d body=%s", response.Code, response.Body.String())
	}

	scanner.panic = true
	response = serve(handler, jsonRequest(http.MethodPost, "/test-token/api/scan", `{}`))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d body=%s", response.Code, response.Body.String())
	}
}

type fakeScanner struct {
	scanAgent string
	rescanID  string
	panic     bool
}

func (s *fakeScanner) Scan(_ context.Context, agent string) (source.ScanReport, error) {
	if s.panic {
		panic("test panic")
	}
	s.scanAgent = agent
	return source.ScanReport{Discovered: 1, Imported: 1}, nil
}

func (s *fakeScanner) Rescan(_ context.Context, sessionID string) (store.ImportResult, error) {
	s.rescanID = sessionID
	return store.ImportResult{Changed: true}, nil
}

type queuedJob struct {
	sessionID string
	options   analyze.Options
	kind      string
}

type fakeQueue struct {
	mu   sync.Mutex
	jobs []queuedJob
	err  error
}

func (q *fakeQueue) Enqueue(_ context.Context, sessionID string, options analyze.Options) (<-chan error, error) {
	return q.enqueue(sessionID, options, "analysis")
}

func (q *fakeQueue) EnqueueRetitle(_ context.Context, sessionID string, options analyze.Options) (<-chan error, error) {
	return q.enqueue(sessionID, options, "retitle")
}

func (q *fakeQueue) enqueue(sessionID string, options analyze.Options, kind string) (<-chan error, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return nil, q.err
	}
	q.jobs = append(q.jobs, queuedJob{sessionID: sessionID, options: options, kind: kind})
	done := make(chan error, 1)
	done <- nil
	close(done)
	return done, nil
}

type fakeLauncher struct {
	sessionID   string
	mode        string
	permissions string
	calls       int
}

type fakeCmux struct {
	status       store.CmuxStatus
	refreshCalls int
	pushIDs      []string
	err          error
}

func (c *fakeCmux) Refresh(context.Context) error {
	c.refreshCalls++
	return c.err
}

func (c *fakeCmux) PushTitle(_ context.Context, sessionID string) error {
	c.pushIDs = append(c.pushIDs, sessionID)
	return c.err
}

func (c *fakeCmux) Status() store.CmuxStatus {
	return c.status
}

func (l *fakeLauncher) Launch(_ context.Context, sessionID, mode, permissions string) (LaunchResult, error) {
	l.sessionID, l.mode, l.permissions = sessionID, mode, permissions
	l.calls++
	return LaunchResult{Mode: "copy", Command: "codex resume native-1"}, nil
}

func testHandler(t *testing.T) (http.Handler, *store.Store, *fakeScanner, *fakeQueue, *fakeLauncher, *bytes.Buffer) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	started := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	session := store.Session{
		ID: "session-1", Agent: "codex", NativeSessionID: "native-1",
		SourcePath: "/tmp/session.jsonl", SourceSize: 1, SourceMTime: started,
		SourceHash: "hash", WorkingDirectory: "/work/login",
		StartedAt: started, LastActiveAt: started.Add(time.Hour),
	}
	messages := []store.Message{
		{Sequence: 0, Timestamp: started, Role: "user", Text: "sensitive transcript phrase about login"},
		{Sequence: 1, Timestamp: started.Add(time.Hour), Role: "assistant", Text: "login retry fixed"},
	}
	if _, err := database.ImportSession(context.Background(), session, messages); err != nil {
		t.Fatal(err)
	}
	sequence := 1
	if err := database.ReplaceAnalysis(context.Background(), session.ID, store.Analysis{
		Title: "Login retry investigation", Summary: "Fixed login retry behavior.", Status: "current",
		Provider: "fake", Model: "test", PromptVersion: "v1", AnalyzedAt: started.Add(time.Hour),
		AnalyzedHash: "analysis", AnalyzedThroughSequence: &sequence, AnalyzedThroughAt: started.Add(time.Hour),
		Segments: []store.Segment{{Position: 0, StartSequence: 0, EndSequence: 1, Title: "Login", Summary: "Fixed retry", Detail: "Verified behavior"}},
	}); err != nil {
		t.Fatal(err)
	}
	scanner := &fakeScanner{}
	queue := &fakeQueue{}
	launcher := &fakeLauncher{}
	logs := &bytes.Buffer{}
	handler, err := NewHandler(Config{
		Token: "test-token", Store: database, Scanner: scanner, Queue: queue,
		Launcher: launcher, Logger: slog.New(slog.NewTextHandler(logs, nil)),
		AnalysisDefaults: analyze.Options{
			Provider: "codex-cli", Model: "default-model", PromptVersion: "v1",
			NormalizerVersion: "v1", LeafTargetChars: 12_000, RollupFanout: 8,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, database, scanner, queue, launcher, logs
}

func apiRequest(method, target string, body io.Reader) *http.Request {
	request := httptest.NewRequest(method, target, body)
	request.Host = "127.0.0.1:1234"
	return request
}

func jsonRequest(method, target, body string) *http.Request {
	request := apiRequest(method, target, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:1234")
	return request
}

func serve(handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
}
