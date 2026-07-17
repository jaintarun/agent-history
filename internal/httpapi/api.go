// Package httpapi exposes the loopback JSON API and embedded web UI.
package httpapi

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tarunjain/agent-history/internal/analyze"
	"github.com/tarunjain/agent-history/internal/launch"
	"github.com/tarunjain/agent-history/internal/source"
	"github.com/tarunjain/agent-history/internal/store"
)

const maxJSONBody = 1 << 20

//go:embed assets/*
var webAssets embed.FS

// Scanner is the transcript-import boundary used by mutation handlers.
type Scanner interface {
	Scan(context.Context, string) (source.ScanReport, error)
	Rescan(context.Context, string) (store.ImportResult, error)
}

// AnalysisQueue is the serialized analysis boundary.
type AnalysisQueue interface {
	Enqueue(context.Context, string, analyze.Options) (<-chan error, error)
	EnqueueRetitle(context.Context, string, analyze.Options) (<-chan error, error)
}

// Launcher is the trusted session-launch boundary.
type Launcher interface {
	Launch(context.Context, string, string, string) (LaunchResult, error)
}

// Cmux controls live cmux reconciliation and explicit title writes.
type Cmux interface {
	Refresh(context.Context) error
	PushTitle(context.Context, string) error
	Status() store.CmuxStatus
}

// LaunchResult is safe structured launch/copy output.
type LaunchResult = launch.Result

// Config supplies API dependencies and process-local security state.
type Config struct {
	Token            string
	PlainURL         bool
	Store            *store.Store
	Scanner          Scanner
	Queue            AnalysisQueue
	Launcher         Launcher
	Cmux             Cmux
	Logger           *slog.Logger
	AnalysisDefaults analyze.Options
}

type handler struct {
	token            string
	prefix           string
	store            *store.Store
	scanner          Scanner
	queue            AnalysisQueue
	launcher         Launcher
	cmux             Cmux
	logger           *slog.Logger
	analysisDefaults analyze.Options
	mux              *http.ServeMux
}

// NewToken returns an unguessable URL path token.
func NewToken() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// NewHandler creates the loopback API, optionally at the unprefixed root.
func NewHandler(config Config) (http.Handler, error) {
	if (!config.PlainURL && config.Token == "") || strings.Contains(config.Token, "/") {
		return nil, errors.New("httpapi: invalid URL token")
	}
	if config.PlainURL && config.Token != "" {
		return nil, errors.New("httpapi: plain URL cannot use a URL token")
	}
	if config.Store == nil {
		return nil, errors.New("httpapi: store is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	prefix := ""
	if config.Token != "" {
		prefix = "/" + config.Token
	}
	h := &handler{
		token: config.Token, prefix: prefix, store: config.Store,
		scanner: config.Scanner, queue: config.Queue, launcher: config.Launcher, cmux: config.Cmux,
		logger: config.Logger, analysisDefaults: config.AnalysisDefaults,
		mux: http.NewServeMux(),
	}
	h.routes()
	return h, nil
}

func (h *handler) routes() {
	h.mux.HandleFunc("GET /{$}", h.index)
	h.mux.HandleFunc("GET /assets/app.css", h.css)
	h.mux.HandleFunc("GET /assets/app.js", h.javascript)
	h.mux.HandleFunc("GET /api/health", h.health)
	h.mux.HandleFunc("GET /api/sessions", h.searchSessions)
	h.mux.HandleFunc("GET /api/sessions/facets", h.facets)
	h.mux.HandleFunc("GET /api/sessions/{id}", h.sessionDetail)
	h.mux.HandleFunc("GET /api/sessions/{id}/messages", h.messages)
	h.mux.HandleFunc("POST /api/sessions/{id}/analyze", h.analyze)
	h.mux.HandleFunc("POST /api/sessions/{id}/retitle", h.retitle)
	h.mux.HandleFunc("DELETE /api/sessions/{id}/analysis", h.deleteAnalysis)
	h.mux.HandleFunc("POST /api/retitle-weak", h.retitleWeak)
	h.mux.HandleFunc("POST /api/sessions/{id}/rescan", h.rescan)
	h.mux.HandleFunc("POST /api/sessions/{id}/launch", h.launch)
	h.mux.HandleFunc("POST /api/sessions/{id}/cmux-title", h.pushCmuxTitle)
	h.mux.HandleFunc("POST /api/scan", h.scan)
	h.mux.HandleFunc("POST /api/cmux/refresh", h.refreshCmux)
	h.mux.HandleFunc("GET /api/settings", h.getSettings)
	h.mux.HandleFunc("PUT /api/settings", h.putSettings)
}

func (h *handler) index(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/index.html", "text/html; charset=utf-8", "no-store")
}

func (h *handler) css(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/app.css", "text/css; charset=utf-8", "no-store")
}

func (h *handler) javascript(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/app.js", "text/javascript; charset=utf-8", "no-store")
}

func (h *handler) webAsset(response http.ResponseWriter, name, contentType, cacheControl string) {
	content, err := webAssets.ReadFile(name)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "asset_error", "embedded asset is unavailable")
		return
	}
	response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
	response.Header().Set("Cache-Control", cacheControl)
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = response.Write(content)
}

func (h *handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if h.prefix != "" && request.URL.Path != h.prefix && !strings.HasPrefix(request.URL.Path, h.prefix+"/") {
		http.NotFound(response, request)
		return
	}
	recorder := &statusRecorder{ResponseWriter: response, status: http.StatusOK}
	requestID, _ := NewToken()
	recorder.Header().Set("X-Request-ID", requestID)
	started := time.Now()
	defer func() {
		if recovered := recover(); recovered != nil {
			writeError(recorder, http.StatusInternalServerError, "internal_error", "internal server error")
		}
		h.logger.Info("http request",
			"request_id", requestID, "method", request.Method,
			"path", strings.TrimPrefix(request.URL.Path, h.prefix),
			"status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
	}()
	if !loopbackHost(request.Host) {
		writeError(recorder, http.StatusForbidden, "invalid_host", "request Host is not loopback")
		return
	}
	if stateChanging(request.Method) && !validOrigin(request) {
		writeError(recorder, http.StatusForbidden, "invalid_origin", "request Origin is not allowed")
		return
	}
	if (request.Method == http.MethodPost || request.Method == http.MethodPut || request.Method == http.MethodPatch) && !jsonContentType(request.Header.Get("Content-Type")) {
		writeError(recorder, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	request = request.Clone(request.Context())
	request.URL.Path = strings.TrimPrefix(request.URL.Path, h.prefix)
	h.mux.ServeHTTP(recorder, request)
}

func (h *handler) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) searchSessions(response http.ResponseWriter, request *http.Request) {
	query, err := parseSearchQuery(request.URL.Query())
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	result, err := h.store.SearchSessions(request.Context(), query)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	sessions := make([]sessionResponse, 0, len(result.Hits))
	for _, hit := range result.Hits {
		item, err := h.sessionDTO(request.Context(), hit.Session)
		if err != nil {
			writeError(response, http.StatusInternalServerError, "store_error", "could not load cmux session state")
			return
		}
		item.Snippet = hit.Snippet
		sessions = append(sessions, item)
	}
	writeJSON(response, http.StatusOK, map[string]any{"sessions": sessions, "next_cursor": result.NextCursor})
}

func (h *handler) facets(response http.ResponseWriter, request *http.Request) {
	facets, err := h.store.SessionFacets(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "store_error", "could not load facets")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"agents": facets.Agents, "directories": facets.Directories,
		"analysis_states": facets.AnalysisStates,
		"started_min":     optionalTime(facets.StartedMin), "last_active_max": optionalTime(facets.LastActiveMax),
	})
}

func (h *handler) sessionDetail(response http.ResponseWriter, request *http.Request) {
	detail, err := h.store.GetSession(request.Context(), request.PathValue("id"))
	if err != nil {
		h.storeError(response, err)
		return
	}
	result, err := h.sessionDTO(request.Context(), detail.Session)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "store_error", "could not load cmux session state")
		return
	}
	for _, segment := range detail.Segments {
		result.Topics = append(result.Topics, topicResponse{
			Position: segment.Position, StartSequence: segment.StartSequence,
			EndSequence: segment.EndSequence, Title: segment.Title,
			Summary: segment.Summary, Detail: segment.Detail,
		})
	}
	writeJSON(response, http.StatusOK, result)
}

func (h *handler) messages(response http.ResponseWriter, request *http.Request) {
	detail, err := h.store.GetSession(request.Context(), request.PathValue("id"))
	if err != nil {
		h.storeError(response, err)
		return
	}
	includeTools := request.URL.Query().Get("include_tools") != "false"
	messages := make([]messageResponse, 0, len(detail.Messages))
	for _, message := range detail.Messages {
		if message.Role == "tool" && !includeTools {
			continue
		}
		messages = append(messages, messageResponse{
			Sequence: message.Sequence, Timestamp: formatAPITime(message.Timestamp),
			Role: message.Role, Text: message.Text, ToolName: message.ToolName,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"messages": messages})
}

func (h *handler) analyze(response http.ResponseWriter, request *http.Request) {
	if h.queue == nil {
		writeError(response, http.StatusNotImplemented, "analysis_unavailable", "analysis is not configured")
		return
	}
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Full     bool   `json:"full"`
	}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	settings, err := h.settings(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not load analysis settings")
		return
	}
	options := h.analysisDefaults
	options.Provider = settings.AnalysisProvider
	options.Model = settings.AnalysisModel
	if body.Provider != "" {
		if body.Provider != h.analysisDefaults.Provider {
			writeError(response, http.StatusBadRequest, "invalid_provider", "analysis provider is not configured")
			return
		}
		options.Provider = body.Provider
	}
	if body.Model != "" {
		if len(body.Model) > 200 {
			writeError(response, http.StatusBadRequest, "invalid_model", "model name is too long")
			return
		}
		options.Model = body.Model
	}
	options.Full = body.Full
	if _, err := h.queue.Enqueue(request.Context(), request.PathValue("id"), options); err != nil {
		if errors.Is(err, analyze.ErrAlreadyQueued) {
			writeError(response, http.StatusConflict, "already_queued", err.Error())
		} else if errors.Is(err, analyze.ErrNoVisibleMessages) {
			writeError(response, http.StatusUnprocessableEntity, "no_visible_messages", err.Error())
		} else if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, "not_found", "session not found")
		} else {
			writeError(response, http.StatusServiceUnavailable, "analysis_error", err.Error())
		}
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (h *handler) retitle(response http.ResponseWriter, request *http.Request) {
	if h.queue == nil {
		writeError(response, http.StatusNotImplemented, "analysis_unavailable", "analysis is not configured")
		return
	}
	var body struct{}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	options, err := h.savedAnalysisOptions(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not load analysis settings")
		return
	}
	if _, err := h.queue.EnqueueRetitle(request.Context(), request.PathValue("id"), options); err != nil {
		h.retitleError(response, err)
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (h *handler) retitleWeak(response http.ResponseWriter, request *http.Request) {
	if h.queue == nil {
		writeError(response, http.StatusNotImplemented, "analysis_unavailable", "analysis is not configured")
		return
	}
	var body struct{}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	ids, err := h.store.WeakTitleSessionIDs(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "store_error", "could not find weak titles")
		return
	}
	options, err := h.savedAnalysisOptions(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not load analysis settings")
		return
	}
	queued := 0
	for _, id := range ids {
		if _, err := h.queue.EnqueueRetitle(request.Context(), id, options); err != nil {
			if !errors.Is(err, analyze.ErrAlreadyQueued) {
				h.logger.Error("could not queue weak-title retitle", "session_id", id, "error", err)
			}
			continue
		}
		queued++
	}
	writeJSON(response, http.StatusAccepted, map[string]int{"matched": len(ids), "queued": queued})
}

func (h *handler) savedAnalysisOptions(ctx context.Context) (analyze.Options, error) {
	settings, err := h.settings(ctx)
	if err != nil {
		return analyze.Options{}, err
	}
	options := h.analysisDefaults
	options.Provider = settings.AnalysisProvider
	options.Model = settings.AnalysisModel
	options.Full = false
	return options, nil
}

func (h *handler) retitleError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, analyze.ErrAlreadyQueued):
		writeError(response, http.StatusConflict, "already_queued", err.Error())
	case errors.Is(err, analyze.ErrNoTopics):
		writeError(response, http.StatusUnprocessableEntity, "no_topics", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(response, http.StatusNotFound, "not_found", "session not found")
	default:
		writeError(response, http.StatusServiceUnavailable, "analysis_error", err.Error())
	}
}

func (h *handler) deleteAnalysis(response http.ResponseWriter, request *http.Request) {
	if err := h.store.DeleteAnalysis(request.Context(), request.PathValue("id")); err != nil {
		h.storeError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (h *handler) scan(response http.ResponseWriter, request *http.Request) {
	if h.scanner == nil {
		writeError(response, http.StatusNotImplemented, "scan_unavailable", "scanner is not configured")
		return
	}
	var body struct {
		Agent string `json:"agent"`
	}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.Agent == "" {
		body.Agent = "all"
	}
	if body.Agent != "all" && body.Agent != "codex" && body.Agent != "claude" {
		writeError(response, http.StatusBadRequest, "invalid_agent", "agent must be all, codex, or claude")
		return
	}
	report, err := h.scanner.Scan(request.Context(), body.Agent)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "scan_error", err.Error())
		return
	}
	writeJSON(response, http.StatusOK, report)
}

func (h *handler) rescan(response http.ResponseWriter, request *http.Request) {
	if h.scanner == nil {
		writeError(response, http.StatusNotImplemented, "scan_unavailable", "scanner is not configured")
		return
	}
	var body struct{}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	result, err := h.scanner.Rescan(request.Context(), request.PathValue("id"))
	if err != nil {
		h.storeError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (h *handler) launch(response http.ResponseWriter, request *http.Request) {
	if h.launcher == nil {
		writeError(response, http.StatusNotImplemented, "launch_unavailable", "launcher is not configured")
		return
	}
	var body struct {
		Launcher    string          `json:"launcher"`
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.Launcher == "" {
		body.Launcher = "auto"
	}
	permissions := launch.PermissionNormal
	if len(body.Permissions) > 0 {
		var supplied *string
		if err := json.Unmarshal(body.Permissions, &supplied); err != nil || supplied == nil {
			writeError(response, http.StatusBadRequest, "invalid_permissions", "permissions must be normal or bypass")
			return
		}
		permissions = *supplied
	}
	if body.Launcher != "auto" && body.Launcher != "cmux" && body.Launcher != "copy" {
		writeError(response, http.StatusBadRequest, "invalid_launcher", "launcher must be auto, cmux, or copy")
		return
	}
	if permissions != launch.PermissionNormal && permissions != launch.PermissionBypass {
		writeError(response, http.StatusBadRequest, "invalid_permissions", "permissions must be normal or bypass")
		return
	}
	result, err := h.launcher.Launch(request.Context(), request.PathValue("id"), body.Launcher, permissions)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, "not_found", "session not found")
		} else {
			writeError(response, http.StatusBadRequest, "launch_error", err.Error())
		}
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (h *handler) refreshCmux(response http.ResponseWriter, request *http.Request) {
	if h.cmux == nil {
		writeError(response, http.StatusNotImplemented, "cmux_unavailable", "cmux integration is not configured")
		return
	}
	var body struct{}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := h.cmux.Refresh(request.Context()); err != nil {
		if h.cmux.Status().Available {
			writeError(response, http.StatusInternalServerError, "cmux_reconciliation_failed", "cmux reconciliation could not be completed")
			return
		}
		writeError(response, http.StatusServiceUnavailable, "cmux_unavailable", "cmux is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "refreshed"})
}

func (h *handler) pushCmuxTitle(response http.ResponseWriter, request *http.Request) {
	if h.cmux == nil {
		writeError(response, http.StatusNotImplemented, "cmux_unavailable", "cmux integration is not configured")
		return
	}
	var body struct{}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	sessionID := request.PathValue("id")
	if _, err := h.store.GetSession(request.Context(), sessionID); err != nil {
		h.storeError(response, err)
		return
	}
	if err := h.cmux.PushTitle(request.Context(), sessionID); err != nil {
		message := err.Error()
		if message == "session is not open in cmux" || message == "session has no Agent History title" {
			writeError(response, http.StatusConflict, "cmux_conflict", message)
		} else {
			writeError(response, http.StatusServiceUnavailable, "cmux_unavailable", "cmux title could not be updated")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "synced"})
}

type settingsResponse struct {
	AnalysisProvider string `json:"analysis_provider"`
	AnalysisModel    string `json:"analysis_model"`
	AnalysisAuto     bool   `json:"analysis_auto"`
	CmuxTitleSync    bool   `json:"cmux_title_sync"`
	CmuxAvailable    bool   `json:"cmux_available"`
	CmuxAccessMode   string `json:"cmux_access_mode"`
	CmuxError        string `json:"cmux_error"`
	CmuxObservedAt   string `json:"cmux_observed_at,omitempty"`
}

func (h *handler) getSettings(response http.ResponseWriter, request *http.Request) {
	settings, err := h.settings(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not load settings")
		return
	}
	writeJSON(response, http.StatusOK, settings)
}

func (h *handler) putSettings(response http.ResponseWriter, request *http.Request) {
	current, err := h.settings(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not load settings")
		return
	}
	var body struct {
		AnalysisProvider string `json:"analysis_provider"`
		AnalysisModel    string `json:"analysis_model"`
		AnalysisAuto     *bool  `json:"analysis_auto"`
		CmuxTitleSync    *bool  `json:"cmux_title_sync"`
	}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.AnalysisProvider != "" {
		current.AnalysisProvider = body.AnalysisProvider
	}
	if body.AnalysisModel != "" {
		current.AnalysisModel = body.AnalysisModel
	}
	if body.AnalysisAuto != nil {
		current.AnalysisAuto = *body.AnalysisAuto
	}
	if body.CmuxTitleSync != nil {
		current.CmuxTitleSync = *body.CmuxTitleSync
	}
	if current.AnalysisProvider != h.analysisDefaults.Provider || strings.TrimSpace(current.AnalysisModel) == "" || len(current.AnalysisModel) > 200 {
		writeError(response, http.StatusBadRequest, "invalid_settings", "provider or model is not configured")
		return
	}
	if err := h.store.SetSettings(request.Context(), map[string]string{
		"analysis.provider": current.AnalysisProvider,
		"analysis.model":    current.AnalysisModel,
		"analysis.auto":     strconv.FormatBool(current.AnalysisAuto),
		"cmux.title_sync":   strconv.FormatBool(current.CmuxTitleSync),
	}); err != nil {
		writeError(response, http.StatusInternalServerError, "settings_error", "could not save settings")
		return
	}
	writeJSON(response, http.StatusOK, current)
}

func (h *handler) settings(ctx context.Context) (settingsResponse, error) {
	result := settingsResponse{
		AnalysisProvider: h.analysisDefaults.Provider,
		AnalysisModel:    h.analysisDefaults.Model,
		AnalysisAuto:     true,
	}
	for key, destination := range map[string]*string{
		"analysis.provider": &result.AnalysisProvider,
		"analysis.model":    &result.AnalysisModel,
	} {
		if value, ok, err := h.store.Setting(ctx, key); err != nil {
			return settingsResponse{}, err
		} else if ok {
			*destination = value
		}
	}
	if value, ok, err := h.store.Setting(ctx, "analysis.auto"); err != nil {
		return settingsResponse{}, err
	} else if ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return settingsResponse{}, err
		}
		result.AnalysisAuto = parsed
	}
	if value, ok, err := h.store.Setting(ctx, "cmux.title_sync"); err != nil {
		return settingsResponse{}, err
	} else if ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return settingsResponse{}, err
		}
		result.CmuxTitleSync = parsed
	}
	status, err := h.store.CmuxStatus(ctx)
	if err != nil {
		return settingsResponse{}, err
	}
	if h.cmux != nil {
		status = h.cmux.Status()
	}
	result.CmuxAvailable = status.Available
	result.CmuxAccessMode = status.AccessMode
	result.CmuxError = status.Error
	if !status.ObservedAt.IsZero() {
		result.CmuxObservedAt = formatAPITime(status.ObservedAt)
	}
	return result, nil
}

func (h *handler) storeError(response http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(response, http.StatusNotFound, "not_found", "session not found")
		return
	}
	writeError(response, http.StatusInternalServerError, "store_error", "storage operation failed")
}

type sessionResponse struct {
	ID                      string          `json:"id"`
	Agent                   string          `json:"agent"`
	NativeSessionID         string          `json:"native_session_id"`
	SourcePath              string          `json:"source_path"`
	WorkingDirectory        string          `json:"working_directory"`
	Title                   string          `json:"title"`
	Summary                 string          `json:"summary"`
	TopicCount              *int            `json:"topic_count"`
	MultipleTopics          bool            `json:"multiple_topics"`
	StartedAt               string          `json:"started_at"`
	LastActiveAt            string          `json:"last_active_at"`
	SpanSeconds             int64           `json:"span_seconds"`
	AnalysisStatus          string          `json:"analysis_status"`
	AnalysisError           string          `json:"analysis_error,omitempty"`
	AnalysisProvider        string          `json:"analysis_provider,omitempty"`
	AnalysisModel           string          `json:"analysis_model,omitempty"`
	AnalysisPromptVersion   string          `json:"analysis_prompt_version,omitempty"`
	AnalyzedAt              *string         `json:"analyzed_at,omitempty"`
	AnalyzedThroughSequence *int            `json:"analyzed_through_sequence,omitempty"`
	AnalyzedThroughAt       *string         `json:"analyzed_through_at,omitempty"`
	Snippet                 string          `json:"snippet,omitempty"`
	Topics                  []topicResponse `json:"topics,omitempty"`
	Cmux                    *cmuxResponse   `json:"cmux,omitempty"`
}

type cmuxResponse struct {
	Open           bool   `json:"open"`
	Lifecycle      string `json:"lifecycle"`
	WorkspaceTitle string `json:"workspace_title"`
	SurfaceTitle   string `json:"surface_title"`
	Target         string `json:"target,omitempty"`
	TargetTitle    string `json:"target_title,omitempty"`
	TitleState     string `json:"title_state"`
	ObservedAt     string `json:"observed_at"`
}

type topicResponse struct {
	Position      int    `json:"position"`
	StartSequence int    `json:"start_sequence"`
	EndSequence   int    `json:"end_sequence"`
	Title         string `json:"title"`
	Summary       string `json:"summary"`
	Detail        string `json:"detail"`
}

type messageResponse struct {
	Sequence  int    `json:"sequence"`
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	ToolName  string `json:"tool_name,omitempty"`
}

func sessionDTO(session store.Session) sessionResponse {
	result := sessionResponse{
		ID: session.ID, Agent: session.Agent, NativeSessionID: session.NativeSessionID,
		SourcePath: session.SourcePath, WorkingDirectory: session.WorkingDirectory,
		Title: session.Title, Summary: session.Summary,
		MultipleTopics: session.TopicCount > 1,
		StartedAt:      formatAPITime(session.StartedAt), LastActiveAt: formatAPITime(session.LastActiveAt),
		SpanSeconds:    int64(session.LastActiveAt.Sub(session.StartedAt).Seconds()),
		AnalysisStatus: session.AnalysisStatus, AnalysisError: session.AnalysisError,
		AnalysisProvider: session.AnalysisProvider, AnalysisModel: session.AnalysisModel,
		AnalysisPromptVersion:   session.AnalysisPromptVersion,
		AnalyzedThroughSequence: session.AnalyzedThroughSequence,
	}
	if session.AnalysisStatus != "none" || session.Title != "" {
		count := session.TopicCount
		result.TopicCount = &count
	}
	if !session.AnalyzedAt.IsZero() {
		value := formatAPITime(session.AnalyzedAt)
		result.AnalyzedAt = &value
	}
	if !session.AnalyzedThroughAt.IsZero() {
		value := formatAPITime(session.AnalyzedThroughAt)
		result.AnalyzedThroughAt = &value
	}
	return result
}

func (h *handler) sessionDTO(ctx context.Context, session store.Session) (sessionResponse, error) {
	result := sessionDTO(session)
	state, ok, err := h.store.CmuxState(ctx, session.ID)
	if err != nil || !ok {
		return result, err
	}
	cmux := &cmuxResponse{
		Open: state.Open, Lifecycle: state.Lifecycle,
		WorkspaceTitle: state.WorkspaceTitle, SurfaceTitle: state.SurfaceTitle,
		TitleState: "not_open", ObservedAt: formatAPITime(state.ObservedAt),
	}
	if state.Open {
		count, err := h.store.OpenCmuxSessionsInWorkspace(ctx, state.WorkspaceID)
		if err != nil {
			return result, err
		}
		cmux.Target = "workspace"
		cmux.TargetTitle = state.WorkspaceTitle
		if count > 1 {
			cmux.Target = "tab"
			cmux.TargetTitle = state.SurfaceTitle
		}
		title := strings.TrimSpace(session.Title)
		switch {
		case title == "":
			cmux.TitleState = "no_agent_title"
		case title == strings.TrimSpace(cmux.TargetTitle):
			cmux.TitleState = "synced"
		default:
			cmux.TitleState = "different"
		}
	}
	result.Cmux = cmux
	return result, nil
}

func parseSearchQuery(values url.Values) (store.SearchQuery, error) {
	query := store.SearchQuery{
		Text: values.Get("q"), Agent: values.Get("agent"), CWD: values.Get("cwd"),
		TopicMode: values.Get("topic_mode"), AnalysisStatus: values.Get("analysis_status"),
		Cmux: values.Get("cmux"), Sort: values.Get("sort"), Cursor: values.Get("cursor"),
	}
	for key, destination := range map[string]**time.Time{
		"active_after": &query.ActiveAfter, "active_before": &query.ActiveBefore,
		"started_after": &query.StartedAfter, "started_before": &query.StartedBefore,
	} {
		if value := values.Get(key); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return store.SearchQuery{}, fmt.Errorf("%s must be RFC 3339", key)
			}
			*destination = &parsed
		}
	}
	if value := values.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return store.SearchQuery{}, errors.New("limit must be an integer")
		}
		query.Limit = limit
	}
	return query, nil
}

func decodeJSONBody(response http.ResponseWriter, request *http.Request, destination any) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxJSONBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func loopbackHost(value string) bool {
	host := value
	if parsed, _, err := net.SplitHostPort(value); err == nil {
		host = parsed
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validOrigin(request *http.Request) bool {
	value := request.Header.Get("Origin")
	if value == "" {
		return true
	}
	origin, err := url.Parse(value)
	return err == nil && origin.Scheme == "http" && strings.EqualFold(origin.Host, request.Host)
}

func stateChanging(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, code, message string) {
	writeJSON(response, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func formatAPITime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return formatAPITime(value)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
