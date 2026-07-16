// Package cmux integrates with the local cmux control socket and documented
// agent hook state without depending on transcript contents.
package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"
)

const defaultTimeout = 2 * time.Second

// Client is a bounded v2 client for cmux's newline-delimited Unix-socket API.
type Client struct {
	socketPath string
	timeout    time.Duration
	nextID     atomic.Uint64
}

// Capabilities describes the control socket access mode and available methods.
type Capabilities struct {
	AccessMode string   `json:"access_mode"`
	Methods    []string `json:"methods"`
}

// Workspace is the cmux workspace metadata needed for title reconciliation.
type Workspace struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	CustomTitle    string `json:"custom_title"`
	HasCustomTitle bool   `json:"has_custom_title"`
	CustomColor    string `json:"custom_color"`
}

// Surface is one exact cmux tab/surface in a workspace.
type Surface struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// NewClient creates a cmux client. An empty path uses standard cmux locations.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath, timeout: defaultTimeout}
}

// Capabilities reads the live socket access mode.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var result Capabilities
	if err := c.call(ctx, "system.capabilities", map[string]any{}, &result); err != nil {
		return Capabilities{}, err
	}
	return result, nil
}

// Workspaces lists all open workspaces.
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error) {
	var result struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := c.call(ctx, "workspace.list", map[string]any{}, &result); err != nil {
		return nil, err
	}
	return result.Workspaces, nil
}

// Surfaces lists exact tabs/surfaces in one workspace.
func (c *Client) Surfaces(ctx context.Context, workspaceID string) ([]Surface, error) {
	var result struct {
		Surfaces []Surface `json:"surfaces"`
	}
	if err := c.call(ctx, "surface.list", map[string]any{"workspace_id": workspaceID}, &result); err != nil {
		return nil, err
	}
	return result.Surfaces, nil
}

// RenameWorkspace assigns a user-owned title to one exact workspace.
func (c *Client) RenameWorkspace(ctx context.Context, workspaceID, title string) error {
	return c.call(ctx, "workspace.rename", map[string]any{
		"workspace_id": workspaceID,
		"title":        title,
	}, &struct{}{})
}

// SetWorkspaceColor assigns one exact hex color to a cmux workspace.
func (c *Client) SetWorkspaceColor(ctx context.Context, workspaceID, color string) error {
	return c.call(ctx, "workspace.action", map[string]any{
		"action":       "set_color",
		"workspace_id": workspaceID,
		"color":        color,
	}, &struct{}{})
}

// RenameSurface assigns a user-owned title to one exact tab/surface.
func (c *Client) RenameSurface(ctx context.Context, workspaceID, surfaceID, title string) error {
	return c.call(ctx, "tab.action", map[string]any{
		"action":       "rename",
		"workspace_id": workspaceID,
		"surface_id":   surfaceID,
		"title":        title,
	}, &struct{}{})
}

func (c *Client) call(ctx context.Context, method string, params map[string]any, destination any) error {
	path, err := c.resolveSocketPath()
	if err != nil {
		return err
	}
	timeout := c.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(requestCtx, "unix", path)
	if err != nil {
		return fmt.Errorf("connect to cmux: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(timeout)
	if requestDeadline, ok := requestCtx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set cmux deadline: %w", err)
	}

	id := c.nextID.Add(1)
	request := struct {
		ID     uint64         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}{ID: id, Method: method, Params: params}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return fmt.Errorf("send cmux %s: %w", method, err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read cmux %s: %w", method, err)
	}
	var response struct {
		ID     uint64          `json:"id"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("decode cmux %s response: %w", method, err)
	}
	if response.ID != id {
		return fmt.Errorf("cmux %s response id %d does not match request %d", method, response.ID, id)
	}
	if !response.OK {
		return fmt.Errorf("cmux %s: %s", method, cmuxErrorMessage(response.Error))
	}
	if destination == nil || len(response.Result) == 0 || string(response.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(response.Result, destination); err != nil {
		return fmt.Errorf("decode cmux %s result: %w", method, err)
	}
	return nil
}

func (c *Client) resolveSocketPath() (string, error) {
	if c.socketPath != "" {
		return c.socketPath, nil
	}
	if path := os.Getenv("CMUX_SOCKET_PATH"); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve cmux socket: %w", err)
	}
	candidates := []string{
		filepath.Join(home, ".local", "state", "cmux", "cmux-"+strconv.Itoa(os.Getuid())+".sock"),
		filepath.Join(home, ".local", "state", "cmux", "cmux.sock"),
		"/tmp/cmux.sock",
	}
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && info.Mode()&os.ModeSocket != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("cmux socket is unavailable")
}

func cmuxErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "request failed"
	}
	var message string
	if json.Unmarshal(raw, &message) == nil && message != "" {
		return message
	}
	var value struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &value) == nil {
		if value.Message != "" {
			return value.Message
		}
		if value.Code != "" {
			return value.Code
		}
	}
	return "request failed"
}
