package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestClientListsCapabilitiesWorkspacesAndSurfaces(t *testing.T) {
	server := newRPCServer(t, []rpcExpectation{
		{method: "system.capabilities", result: map[string]any{"access_mode": "allowAll", "methods": []string{"workspace.list"}}},
		{method: "workspace.list", result: map[string]any{"workspaces": []map[string]any{{
			"id": "w1", "title": "Workspace", "custom_title": "Workspace",
			"has_custom_title": true, "custom_color": "#196F3D", "description": "Short summary",
		}}}},
		{method: "surface.list", params: map[string]any{"workspace_id": "w1"}, result: map[string]any{"surfaces": []map[string]any{{"id": "s1", "title": "Tab", "type": "terminal"}}}},
	})
	client := NewClient(server.path)

	capabilities, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.AccessMode != "allowAll" || len(capabilities.Methods) != 1 {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	workspaces, err := client.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0].ID != "w1" ||
		!workspaces[0].HasCustomTitle || workspaces[0].CustomColor != "#196F3D" ||
		workspaces[0].Description != "Short summary" {
		t.Fatalf("workspaces = %#v", workspaces)
	}
	surfaces, err := client.Surfaces(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(surfaces) != 1 || surfaces[0].ID != "s1" || surfaces[0].Title != "Tab" {
		t.Fatalf("surfaces = %#v", surfaces)
	}
}

func TestClientSetsExactWorkspaceColor(t *testing.T) {
	server := newRPCServer(t, []rpcExpectation{{
		method: "workspace.action",
		params: map[string]any{
			"action": "set_color", "workspace_id": "w1", "color": "#A04000",
		},
		result: map[string]any{"workspace_id": "w1", "color": "#A04000"},
	}})

	if err := NewClient(server.path).SetWorkspaceColor(
		context.Background(), "w1", "#A04000",
	); err != nil {
		t.Fatal(err)
	}
}

func TestClientSetsExactWorkspaceDescription(t *testing.T) {
	server := newRPCServer(t, []rpcExpectation{{
		method: "workspace.action",
		params: map[string]any{
			"action": "set_description", "workspace_id": "w1", "description": "Short summary",
		},
		result: map[string]any{"workspace_id": "w1", "description": "Short summary"},
	}})

	if err := NewClient(server.path).SetWorkspaceDescription(
		context.Background(), "w1", "Short summary",
	); err != nil {
		t.Fatal(err)
	}
}

func TestClientRenamesExactWorkspaceAndSurface(t *testing.T) {
	server := newRPCServer(t, []rpcExpectation{
		{method: "workspace.rename", params: map[string]any{"workspace_id": "w1", "title": "Generated title"}, result: map[string]any{"workspace_id": "w1"}},
		{method: "tab.action", params: map[string]any{"action": "rename", "workspace_id": "w1", "surface_id": "s1", "title": "Generated title"}, result: map[string]any{"surface_id": "s1"}},
	})
	client := NewClient(server.path)
	if err := client.RenameWorkspace(context.Background(), "w1", "Generated title"); err != nil {
		t.Fatal(err)
	}
	if err := client.RenameSurface(context.Background(), "w1", "s1", "Generated title"); err != nil {
		t.Fatal(err)
	}
}

func TestClientReportsProtocolAndTimeoutErrors(t *testing.T) {
	t.Run("cmux error", func(t *testing.T) {
		server := newRPCServer(t, []rpcExpectation{{method: "system.capabilities", errorMessage: "access denied"}})
		_, err := NewClient(server.path).Capabilities(context.Background())
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		path := shortSocketPath(t)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			connection, acceptErr := listener.Accept()
			if acceptErr == nil {
				defer connection.Close()
				_, _ = bufio.NewReader(connection).ReadBytes('\n')
				_, _ = connection.Write([]byte("not-json\n"))
			}
		}()
		_, err = NewClient(path).Capabilities(context.Background())
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		path := shortSocketPath(t)
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			connection, acceptErr := listener.Accept()
			if acceptErr == nil {
				defer connection.Close()
				_, _ = bufio.NewReader(connection).ReadBytes('\n')
				time.Sleep(200 * time.Millisecond)
			}
		}()
		client := NewClient(path)
		client.timeout = 20 * time.Millisecond
		_, err = client.Capabilities(context.Background())
		if err == nil {
			t.Fatal("Capabilities succeeded, want timeout")
		}
	})
}

type rpcExpectation struct {
	method       string
	params       map[string]any
	result       map[string]any
	errorMessage string
}

type rpcServer struct {
	path string
}

func newRPCServer(t *testing.T, expectations []rpcExpectation) rpcServer {
	t.Helper()
	path := shortSocketPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, expectation := range expectations {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			var request struct {
				ID     any            `json:"id"`
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			decodeErr := json.NewDecoder(bufio.NewReader(connection)).Decode(&request)
			if decodeErr != nil {
				t.Errorf("decode request: %v", decodeErr)
				_ = connection.Close()
				return
			}
			if request.Method != expectation.method {
				t.Errorf("method = %q, want %q", request.Method, expectation.method)
			}
			if expectation.params != nil && !equalJSON(request.Params, expectation.params) {
				t.Errorf("params = %#v, want %#v", request.Params, expectation.params)
			}
			response := map[string]any{"id": request.ID, "ok": expectation.errorMessage == ""}
			if expectation.errorMessage != "" {
				response["error"] = map[string]any{"message": expectation.errorMessage}
			} else {
				response["result"] = expectation.result
			}
			_ = json.NewEncoder(connection).Encode(response)
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	return rpcServer{path: path}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	file, err := os.CreateTemp("/tmp", "agent-history-cmux-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func equalJSON(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}
