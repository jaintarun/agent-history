package cmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tarunjain/agent-history/internal/store"
)

// API is the cmux control boundary used by the reconciler.
type API interface {
	Capabilities(context.Context) (Capabilities, error)
	Workspaces(context.Context) ([]Workspace, error)
	Surfaces(context.Context, string) ([]Surface, error)
	RenameWorkspace(context.Context, string, string) error
	RenameSurface(context.Context, string, string, string) error
}

// Reconciler joins live cmux state to imported sessions and owns title writes.
type Reconciler struct {
	api    API
	store  *store.Store
	home   string
	logger *slog.Logger
	now    func() time.Time
	mu     sync.Mutex
	status store.CmuxStatus
}

type liveSnapshot struct {
	states              map[string]store.CmuxSessionState
	sessionsInWorkspace map[string]int
}

// NewReconciler creates a serialized cmux state and title coordinator.
func NewReconciler(api API, database *store.Store, home string, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{api: api, store: database, home: home, logger: logger, now: time.Now}
}

// Refresh stores current cmux state and applies eligible automatic title sync.
func (r *Reconciler) Refresh(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshLocked(ctx, true)
}

// RefreshWithoutSync refreshes observable state without performing writes.
func (r *Reconciler) RefreshWithoutSync(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.snapshotLocked(ctx)
	return err
}

func (r *Reconciler) refreshLocked(ctx context.Context, allowSync bool) error {
	snapshot, err := r.snapshotLocked(ctx)
	if err != nil {
		return err
	}
	if !allowSync {
		return nil
	}
	enabled, err := r.autoSyncEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	wrote, syncErr := r.syncEligibleTitles(ctx, snapshot)
	if wrote {
		if _, err := r.snapshotLocked(ctx); err != nil {
			syncErr = errors.Join(syncErr, err)
		}
	}
	return syncErr
}

func (r *Reconciler) snapshotLocked(ctx context.Context) (liveSnapshot, error) {
	observedAt := r.now().UTC()
	capabilities, err := r.api.Capabilities(ctx)
	if err != nil {
		status := store.CmuxStatus{Available: false, Error: "cmux is unavailable", ObservedAt: observedAt}
		return liveSnapshot{}, r.failSnapshot(ctx, status, fmt.Errorf("read cmux capabilities: %w", err))
	}
	if capabilities.AccessMode != "allowAll" {
		message := fmt.Sprintf("cmux access mode is %s; allowAll is required", displayAccessMode(capabilities.AccessMode))
		status := store.CmuxStatus{Available: false, AccessMode: capabilities.AccessMode, Error: message, ObservedAt: observedAt}
		return liveSnapshot{}, r.failSnapshot(ctx, status, errors.New(message))
	}
	workspaces, err := r.api.Workspaces(ctx)
	if err != nil {
		status := store.CmuxStatus{Available: false, AccessMode: capabilities.AccessMode, Error: "cmux workspaces could not be read", ObservedAt: observedAt}
		return liveSnapshot{}, r.failSnapshot(ctx, status, fmt.Errorf("list cmux workspaces: %w", err))
	}
	workspaceByID := make(map[string]Workspace, len(workspaces))
	surfaceByWorkspace := make(map[string]map[string]Surface, len(workspaces))
	for _, workspace := range workspaces {
		workspaceByID[workspace.ID] = workspace
		surfaces, err := r.api.Surfaces(ctx, workspace.ID)
		if err != nil {
			status := store.CmuxStatus{Available: false, AccessMode: capabilities.AccessMode, Error: "cmux tabs could not be read", ObservedAt: observedAt}
			return liveSnapshot{}, r.failSnapshot(ctx, status, fmt.Errorf("list cmux surfaces: %w", err))
		}
		byID := make(map[string]Surface, len(surfaces))
		for _, surface := range surfaces {
			byID[surface.ID] = surface
		}
		surfaceByWorkspace[workspace.ID] = byID
	}

	mappings, diagnostics := LoadHookMappings(r.home)
	for _, diagnostic := range diagnostics {
		r.logger.Warn("could not read cmux hook state", "error", diagnostic)
	}
	identities, err := r.store.SessionsByNativeIdentity(ctx)
	if err != nil {
		return liveSnapshot{}, fmt.Errorf("list Agent History identities: %w", err)
	}
	identityByNative := make(map[string]string, len(identities))
	for _, identity := range identities {
		identityByNative[identity.Agent+"\x00"+identity.NativeSessionID] = identity.SessionID
	}
	snapshot := liveSnapshot{
		states:              make(map[string]store.CmuxSessionState),
		sessionsInWorkspace: make(map[string]int),
	}
	for _, mapping := range mappings {
		sessionID, exists := identityByNative[mapping.Agent+"\x00"+mapping.NativeSessionID]
		if !exists {
			continue
		}
		workspace, exists := workspaceByID[mapping.WorkspaceID]
		if !exists {
			continue
		}
		surface, exists := surfaceByWorkspace[mapping.WorkspaceID][mapping.SurfaceID]
		if !exists {
			continue
		}
		state := store.CmuxSessionState{
			SessionID: sessionID, Open: true,
			WorkspaceID: mapping.WorkspaceID, SurfaceID: mapping.SurfaceID,
			WorkspaceTitle: workspace.Title, SurfaceTitle: surface.Title,
			WorkspaceHasCustomTitle: workspace.HasCustomTitle,
			Lifecycle:               mapping.Lifecycle, ObservedAt: observedAt,
		}
		snapshot.states[sessionID] = state
		snapshot.sessionsInWorkspace[mapping.WorkspaceID]++
	}
	states := make([]store.CmuxSessionState, 0, len(snapshot.states))
	for _, state := range snapshot.states {
		states = append(states, state)
	}
	status := store.CmuxStatus{Available: true, AccessMode: capabilities.AccessMode, ObservedAt: observedAt}
	if err := r.store.ReplaceCmuxSnapshot(ctx, status, states); err != nil {
		return liveSnapshot{}, err
	}
	r.status = status
	return snapshot, nil
}

func (r *Reconciler) failSnapshot(ctx context.Context, status store.CmuxStatus, cause error) error {
	if err := r.store.ReplaceCmuxSnapshot(ctx, status, nil); err != nil {
		cause = errors.Join(cause, err)
	}
	r.status = status
	return cause
}

func (r *Reconciler) autoSyncEnabled(ctx context.Context) (bool, error) {
	value, ok, err := r.store.Setting(ctx, "cmux.title_sync")
	if err != nil || !ok {
		return false, err
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid cmux.title_sync setting: %w", err)
	}
	return enabled, nil
}

func (r *Reconciler) syncEligibleTitles(ctx context.Context, snapshot liveSnapshot) (bool, error) {
	var wrote bool
	var syncErrors []error
	for sessionID, observed := range snapshot.states {
		if snapshot.sessionsInWorkspace[observed.WorkspaceID] != 1 {
			continue
		}
		detail, err := r.store.GetSession(ctx, sessionID)
		if err != nil {
			syncErrors = append(syncErrors, err)
			continue
		}
		title := strings.TrimSpace(detail.Session.Title)
		if title == "" || detail.Session.AnalysisStatus != "current" {
			continue
		}
		state, ok, err := r.store.CmuxState(ctx, sessionID)
		if err != nil || !ok {
			if err != nil {
				syncErrors = append(syncErrors, err)
			}
			continue
		}
		if strings.TrimSpace(observed.WorkspaceTitle) != title &&
			(!observed.WorkspaceHasCustomTitle || observed.WorkspaceTitle == state.LastPushedWorkspaceTitle) {
			if err := r.api.RenameWorkspace(ctx, observed.WorkspaceID, title); err != nil {
				syncErrors = append(syncErrors, fmt.Errorf("rename cmux workspace for session %s: %w", sessionID, err))
			} else if err := r.store.RecordCmuxPush(ctx, sessionID, title, "", r.now().UTC()); err != nil {
				syncErrors = append(syncErrors, err)
			} else {
				wrote = true
			}
		}
		if state.LastPushedSurfaceTitle != "" && observed.SurfaceTitle == state.LastPushedSurfaceTitle &&
			strings.TrimSpace(observed.SurfaceTitle) != title {
			if err := r.api.RenameSurface(ctx, observed.WorkspaceID, observed.SurfaceID, title); err != nil {
				syncErrors = append(syncErrors, fmt.Errorf("rename cmux tab for session %s: %w", sessionID, err))
			} else if err := r.store.RecordCmuxPush(ctx, sessionID, "", title, r.now().UTC()); err != nil {
				syncErrors = append(syncErrors, err)
			} else {
				wrote = true
			}
		}
	}
	return wrote, errors.Join(syncErrors...)
}

// PushTitle explicitly writes one generated title to its exact cmux target.
func (r *Reconciler) PushTitle(ctx context.Context, sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot, err := r.snapshotLocked(ctx)
	if err != nil {
		return err
	}
	observed, ok := snapshot.states[sessionID]
	if !ok || !observed.Open {
		return errors.New("session is not open in cmux")
	}
	detail, err := r.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	title := strings.TrimSpace(detail.Session.Title)
	if title == "" {
		return errors.New("session has no Agent History title")
	}
	var pushErrors []error
	if err := r.api.RenameSurface(ctx, observed.WorkspaceID, observed.SurfaceID, title); err != nil {
		pushErrors = append(pushErrors, fmt.Errorf("rename cmux tab: %w", err))
	} else if err := r.store.RecordCmuxPush(ctx, sessionID, "", title, r.now().UTC()); err != nil {
		pushErrors = append(pushErrors, err)
	}
	if snapshot.sessionsInWorkspace[observed.WorkspaceID] == 1 {
		if err := r.api.RenameWorkspace(ctx, observed.WorkspaceID, title); err != nil {
			pushErrors = append(pushErrors, fmt.Errorf("rename cmux workspace: %w", err))
		} else if err := r.store.RecordCmuxPush(ctx, sessionID, title, "", r.now().UTC()); err != nil {
			pushErrors = append(pushErrors, err)
		}
	}
	if _, err := r.snapshotLocked(ctx); err != nil {
		pushErrors = append(pushErrors, err)
	}
	return errors.Join(pushErrors...)
}

// Start performs an immediate refresh and then reconciles at the given cadence.
func (r *Reconciler) Start(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.refreshAndLog(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.refreshAndLog(ctx)
			}
		}
	}()
	return done
}

// Status returns the last in-process cmux availability observation.
func (r *Reconciler) Status() store.CmuxStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Reconciler) refreshAndLog(ctx context.Context) {
	if err := r.Refresh(ctx); err != nil && ctx.Err() == nil {
		r.logger.Warn("cmux reconciliation unavailable", "error", err)
	}
}

func displayAccessMode(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
