// Package httpapi exposes the secured loopback JSON API and embedded web UI.
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
}

// Launcher is the trusted session-launch boundary.
type Launcher interface {
	Launch(context.Context, string, string) (LaunchResult, error)
}

// LaunchResult is safe structured launch/copy output.
type LaunchResult = launch.Result

// Config supplies API dependencies and process-local security state.
type Config struct {
	Token            string
	Store            *store.Store
	Scanner          Scanner
	Queue            AnalysisQueue
	Launcher         Launcher
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

// NewHandler creates the token-prefixed secured loopback API.
func NewHandler(config Config) (http.Handler, error) {
	if config.Token == "" || strings.Contains(config.Token, "/") {
		return nil, errors.New("httpapi: invalid URL token")
	}
	if config.Store == nil {
		return nil, errors.New("httpapi: store is required")
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	h := &handler{
		token: config.Token, prefix: "/" + config.Token, store: config.Store,
		scanner: config.Scanner, queue: config.Queue, launcher: config.Launcher,
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
	h.mux.HandleFunc("DELETE /api/sessions/{id}/analysis", h.deleteAnalysis)
	h.mux.HandleFunc("POST /api/sessions/{id}/rescan", h.rescan)
	h.mux.HandleFunc("POST /api/sessions/{id}/launch", h.launch)
	h.mux.HandleFunc("POST /api/scan", h.scan)
	h.mux.HandleFunc("GET /api/settings", h.getSettings)
	h.mux.HandleFunc("PUT /api/settings", h.putSettings)
}

func (h *handler) index(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/index.html", "text/html; charset=utf-8", "no-store")
}

func (h *handler) css(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/app.css", "text/css; charset=utf-8", "public, max-age=3600")
}

func (h *handler) javascript(response http.ResponseWriter, _ *http.Request) {
	h.webAsset(response, "assets/app.js", "text/javascript; charset=utf-8", "public, max-age=3600")
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
	if request.URL.Path != h.prefix && !strings.HasPrefix(request.URL.Path, h.prefix+"/") {
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
		item := sessionDTO(hit.Session)
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
	result := sessionDTO(detail.Session)
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
		} else if errors.Is(err, store.ErrNotFound) {
			writeError(response, http.StatusNotFound, "not_found", "session not found")
		} else {
			writeError(response, http.StatusServiceUnavailable, "analysis_error", err.Error())
		}
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]string{"status": "queued"})
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
		Launcher string `json:"launcher"`
	}
	if err := decodeJSONBody(response, request, &body); err != nil {
		writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if body.Launcher == "" {
		body.Launcher = "auto"
	}
	if body.Launcher != "auto" && body.Launcher != "cmux" && body.Launcher != "copy" {
		writeError(response, http.StatusBadRequest, "invalid_launcher", "launcher must be auto, cmux, or copy")
		return
	}
	result, err := h.launcher.Launch(request.Context(), request.PathValue("id"), body.Launcher)
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

type settingsResponse struct {
	AnalysisProvider string `json:"analysis_provider"`
	AnalysisModel    string `json:"analysis_model"`
	AnalysisAuto     bool   `json:"analysis_auto"`
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
	if current.AnalysisProvider != h.analysisDefaults.Provider || strings.TrimSpace(current.AnalysisModel) == "" || len(current.AnalysisModel) > 200 {
		writeError(response, http.StatusBadRequest, "invalid_settings", "provider or model is not configured")
		return
	}
	if err := h.store.SetSettings(request.Context(), map[string]string{
		"analysis.provider": current.AnalysisProvider,
		"analysis.model":    current.AnalysisModel,
		"analysis.auto":     strconv.FormatBool(current.AnalysisAuto),
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

func parseSearchQuery(values url.Values) (store.SearchQuery, error) {
	query := store.SearchQuery{
		Text: values.Get("q"), Agent: values.Get("agent"), CWD: values.Get("cwd"),
		TopicMode: values.Get("topic_mode"), AnalysisStatus: values.Get("analysis_status"),
		Sort: values.Get("sort"), Cursor: values.Get("cursor"),
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
