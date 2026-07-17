# Resume Permission Modes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add explicit normal and agent-specific permission-bypass resume actions for stored Codex and Claude Code sessions without accepting browser-supplied commands or flags.

**Architecture:** Keep source adapters responsible only for trusted normal `ResumeSpec` values. Extend the launcher boundary with a fixed `normal|bypass` permission enum, validate the normal specification first, and then apply one application-owned agent-specific flag before using the existing cmux or copy path. The HTTP layer validates and defaults the enum, while the embedded JavaScript renders only the two actions valid for the stored source agent.

**Tech Stack:** Go 1.24, standard `net/http`, embedded HTML/CSS/JavaScript, Node's built-in VM assertion harness, fake subprocess runners, and cmux CLI command generation.

## Global Constraints

- The browser may submit only `launcher: "auto"|"cmux"|"copy"` and `permissions: "normal"|"bypass"`.
- An omitted `permissions` field means `normal`.
- Codex bypass is `codex resume --dangerously-bypass-approvals-and-sandbox <session-id>`.
- Claude bypass is `claude --dangerously-skip-permissions --resume <session-id>`.
- Source adapters continue to return only their existing normal resume specifications.
- Validate the source adapter's exact normal specification before adding a bypass flag.
- Do not persist a default, remember a prior choice, add arbitrary CLI input, or add Chrome/generic-terminal session support.
- Missing cmux continues to return a copyable command; cmux failure remains an error.
- Tests must never launch a real Codex or Claude session.

---

### Task 1: Trusted Launcher Permission Modes

**Files:**
- Modify: `internal/launch/launch.go`
- Modify: `internal/launch/launch_test.go`

**Interfaces:**
- Consumes: existing trusted `source.ResumeSpec`, stored `store.Session`, and `runner`.
- Produces: `launch.PermissionNormal`, `launch.PermissionBypass`, and `(*Launcher).Launch(context.Context, string, string, string) (Result, error)`.
- Invariant: the fourth argument is a fixed permission enum, not a CLI flag.

- [ ] **Step 1: Write failing behavior tests for exact copy commands**

Update every existing `launcher.Launch` test call to pass
`PermissionNormal` as the fourth argument. Replace
`TestCopyCommandsAreSafelyRendered` with a table covering all four commands:

```go
func TestCopyCommandsAreSafelyRendered(t *testing.T) {
	database := openLauncherStore(t)
	cwd := filepath.Join(t.TempDir(), "project's files")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		agent       string
		id          string
		permissions string
		want        string
	}{
		{
			name: "codex normal", agent: "codex",
			id: "11111111-1111-4111-8111-111111111111",
			permissions: PermissionNormal,
			want: "codex resume 11111111-1111-4111-8111-111111111111",
		},
		{
			name: "codex bypass", agent: "codex",
			id: "11111111-1111-4111-8111-111111111111",
			permissions: PermissionBypass,
			want: "codex resume --dangerously-bypass-approvals-and-sandbox 11111111-1111-4111-8111-111111111111",
		},
		{
			name: "claude normal", agent: "claude", id: "session:release.one",
			permissions: PermissionNormal,
			want: "claude --resume session:release.one",
		},
		{
			name: "claude bypass", agent: "claude", id: "session:release.one",
			permissions: PermissionBypass,
			want: "claude --dangerously-skip-permissions --resume session:release.one",
		},
	}
	launcher := newWithRunner(
		database,
		&fakeRunner{lookPathErr: errors.New("missing")},
		codex.New(t.TempDir()),
		claude.New(t.TempDir()),
	)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sessionID := importLaunchSession(t, database, test.agent, test.id, cwd, "Title with 'quote' "+test.name)
			result, err := launcher.Launch(context.Background(), sessionID, "copy", test.permissions)
			if err != nil {
				t.Fatal(err)
			}
			if result.Mode != "copy" || result.Command != test.want || result.Workspace != "" {
				t.Fatalf("copy result = %#v", result)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused test and confirm the red state**

Run:

```sh
go test ./internal/launch -run '^TestCopyCommandsAreSafelyRendered$' -count=1
```

Expected: compilation fails because `PermissionNormal`, `PermissionBypass`, and
the four-argument `Launch` method do not exist yet.

- [ ] **Step 3: Implement the fixed permission transform**

Add exported constants, extend `Launch`, validate the enum before loading the
session, validate the normal `ResumeSpec`, and render a copied/transformed
argument slice:

```go
const (
	PermissionNormal = "normal"
	PermissionBypass = "bypass"
)

func (l *Launcher) Launch(ctx context.Context, sessionID, mode, permissions string) (Result, error) {
	if mode != "auto" && mode != "cmux" && mode != "copy" {
		return Result{}, fmt.Errorf("unsupported launcher mode %q", mode)
	}
	if permissions != PermissionNormal && permissions != PermissionBypass {
		return Result{}, fmt.Errorf("unsupported permission mode %q", permissions)
	}
	detail, err := l.store.GetSession(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	adapter := l.sources[detail.Session.Agent]
	if adapter == nil {
		return Result{}, fmt.Errorf("source adapter %q is not configured", detail.Session.Agent)
	}
	spec, err := adapter.ResumeSpec(detail.Session)
	if err != nil {
		return Result{}, err
	}
	if err := validateSpec(detail.Session, spec); err != nil {
		return Result{}, err
	}
	args := permissionArgs(detail.Session.Agent, spec.Args, permissions)
	command := renderCommand(spec.Executable, args)
	if mode == "copy" {
		return Result{Mode: "copy", Command: command}, nil
	}
	cmux, err := l.runner.LookPath("cmux")
	if err != nil {
		return Result{Mode: "copy", Command: command}, nil
	}
	name := workspaceName(detail.Session)
	args = []string{
		"workspace", "create", "--name", name, "--cwd", spec.CWD,
		"--command", command, "--focus", "true",
	}
	output, err := l.runner.Run(ctx, cmux, args)
	if err != nil {
		return Result{}, fmt.Errorf("launch cmux workspace: %w", err)
	}
	return Result{Mode: "cmux", Workspace: strings.TrimSpace(string(output))}, nil
}

func permissionArgs(agent string, normal []string, permissions string) []string {
	args := append([]string(nil), normal...)
	if permissions == PermissionNormal {
		return args
	}
	switch agent {
	case "codex":
		args = append(args, "")
		copy(args[2:], args[1:])
		args[1] = "--dangerously-bypass-approvals-and-sandbox"
	case "claude":
		args = append([]string{"--dangerously-skip-permissions"}, args...)
	}
	return args
}
```

- [ ] **Step 4: Run copy-command tests and confirm green**

Run:

```sh
gofmt -w internal/launch/launch.go internal/launch/launch_test.go
go test ./internal/launch -run '^TestCopyCommandsAreSafelyRendered$' -count=1
```

Expected: PASS with four subtests.

- [ ] **Step 5: Write failing tests for cmux, invalid modes, and slice isolation**

Convert `TestCmuxLaunchUsesExactTrustedArgv` into a two-case table that keeps the
existing Claude normal expectation and adds a Codex bypass expectation:

```go
tests := []struct {
	name        string
	agent       string
	permissions string
	command     string
}{
	{
		name: "claude normal", agent: "claude", permissions: PermissionNormal,
		command: "claude --resume safe-session",
	},
	{
		name: "codex bypass", agent: "codex", permissions: PermissionBypass,
		command: "codex resume --dangerously-bypass-approvals-and-sandbox safe-session",
	},
}
```

For each case, assert the runner receives:

```go
want := []string{
	"workspace", "create", "--name", "Fix user's retry", "--cwd", cwd,
	"--command", test.command, "--focus", "true",
}
```

Add rejection and copy-isolation tests:

```go
func TestLauncherRejectsUnknownPermissionMode(t *testing.T) {
	database := openLauncherStore(t)
	sessionID := importLaunchSession(t, database, "codex", "safe-session", t.TempDir(), "Invalid mode")
	launcher := newWithRunner(database, &fakeRunner{}, codex.New(t.TempDir()))
	if _, err := launcher.Launch(context.Background(), sessionID, "copy", "custom-flag"); err == nil {
		t.Fatal("Launch accepted an unknown permission mode")
	}
}

func TestPermissionArgsDoesNotMutateNormalSpec(t *testing.T) {
	normal := []string{"resume", "safe-session"}
	got := permissionArgs("codex", normal, PermissionBypass)
	if !reflect.DeepEqual(normal, []string{"resume", "safe-session"}) {
		t.Fatalf("normal args mutated: %q", normal)
	}
	want := []string{"resume", "--dangerously-bypass-approvals-and-sandbox", "safe-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bypass args = %q, want %q", got, want)
	}
}
```

- [ ] **Step 6: Run the launcher package and correct only observed failures**

Run:

```sh
gofmt -w internal/launch/launch.go internal/launch/launch_test.go
go test ./internal/launch -count=1
```

Expected: PASS. Do not change `source.ResumeSpec` or either source adapter.

- [ ] **Step 7: Commit the launcher unit**

```sh
git add internal/launch/launch.go internal/launch/launch_test.go
git commit -m "feat: add trusted resume permission modes"
```

---

### Task 2: HTTP Permission Contract

**Files:**
- Modify: `internal/httpapi/api.go`
- Modify: `internal/httpapi/api_test.go`

**Interfaces:**
- Consumes: `launch.PermissionNormal`, `launch.PermissionBypass`, and the
  four-argument launcher method from Task 1.
- Produces: `POST /api/sessions/{id}/launch` with optional
  `permissions: "normal"|"bypass"`.
- Compatibility: missing `permissions` is forwarded as `normal`.

- [ ] **Step 1: Write failing API tests for defaulting and bypass forwarding**

Extend `fakeLauncher` with the permission value and call count:

```go
type fakeLauncher struct {
	sessionID  string
	mode       string
	permissions string
	calls      int
}

func (l *fakeLauncher) Launch(_ context.Context, sessionID, mode, permissions string) (LaunchResult, error) {
	l.sessionID, l.mode, l.permissions = sessionID, mode, permissions
	l.calls++
	return LaunchResult{Mode: "copy", Command: "codex resume native-1"}, nil
}
```

Change the existing launch assertion in `TestSessionMutationWorkflows` to prove
the omitted field defaults to normal:

```go
response = serve(handler, jsonRequest(
	http.MethodPost,
	"/test-token/api/sessions/session-1/launch",
	`{"launcher":"copy"}`,
))
if response.Code != http.StatusOK ||
	launcher.sessionID != "session-1" ||
	launcher.mode != "copy" ||
	launcher.permissions != launch.PermissionNormal {
	t.Fatalf("launch response=%d launcher=%#v body=%s", response.Code, launcher, response.Body.String())
}
```

Add a separate test:

```go
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
```

Import `internal/launch` in the test file.

- [ ] **Step 2: Run the API tests and confirm the red state**

Run:

```sh
go test ./internal/httpapi -run '^(TestSessionMutationWorkflows|TestLaunchPermissionModeIsValidated)$' -count=1
```

Expected: compilation fails because the HTTP launcher interface and handler
still use the three-argument method and never decode `permissions`.

- [ ] **Step 3: Implement decoding, defaulting, and validation**

Change the HTTP launcher boundary:

```go
type Launcher interface {
	Launch(context.Context, string, string, string) (LaunchResult, error)
}
```

Extend the request body and validate it before calling the launcher:

```go
var body struct {
	Launcher    string `json:"launcher"`
	Permissions string `json:"permissions"`
}
if err := decodeJSONBody(response, request, &body); err != nil {
	writeError(response, http.StatusBadRequest, "invalid_json", err.Error())
	return
}
if body.Launcher == "" {
	body.Launcher = "auto"
}
if body.Permissions == "" {
	body.Permissions = launch.PermissionNormal
}
if body.Launcher != "auto" && body.Launcher != "cmux" && body.Launcher != "copy" {
	writeError(response, http.StatusBadRequest, "invalid_launcher", "launcher must be auto, cmux, or copy")
	return
}
if body.Permissions != launch.PermissionNormal && body.Permissions != launch.PermissionBypass {
	writeError(response, http.StatusBadRequest, "invalid_permissions", "permissions must be normal or bypass")
	return
}
result, err := h.launcher.Launch(
	request.Context(),
	request.PathValue("id"),
	body.Launcher,
	body.Permissions,
)
```

Keep the existing not-found, launch-error, and success responses unchanged.

- [ ] **Step 4: Run focused and package tests**

Run:

```sh
gofmt -w internal/httpapi/api.go internal/httpapi/api_test.go
go test ./internal/httpapi -run '^(TestSessionMutationWorkflows|TestLaunchPermissionModeIsValidated)$' -count=1
go test ./internal/httpapi -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the HTTP contract**

```sh
git add internal/httpapi/api.go internal/httpapi/api_test.go
git commit -m "feat: expose resume permission modes"
```

---

### Task 3: Agent-Specific Web Resume Actions

**Files:**
- Modify: `internal/httpapi/assets/app.js`
- Modify: `internal/httpapi/assets/app.css`
- Modify: `internal/httpapi/testdata/app_behavior_test.js`
- Modify: `internal/httpapi/api_test.go`

**Interfaces:**
- Consumes: session detail `agent: "codex"|"claude"` and the Task 2 launch API.
- Produces: two visible actions per supported agent and requests with
  `{launcher:"auto", permissions:"normal"|"bypass"}`.
- UI labels: Codex uses `Resume with YOLO`; Claude uses
  `Resume with dangerously skipped permissions`.

- [ ] **Step 1: Write failing browser behavior assertions**

Add this helper and test to `app_behavior_test.js`:

```js
function childByText(parent, text) {
  return parent.children.find((child) => textOf(child) === text);
}

async function clickNode(node) {
  const event = { currentTarget: node, target: node, preventDefault() {} };
  const results = (node.listeners.get("click") || []).map((listener) => listener(event));
  await Promise.all(results);
  await settle();
}

async function testResumePermissionActions() {
  const environment = createEnvironment("off");
  await settle();
  environment.requests.length = 0;
  environment.fetchHandler = (url) => {
    const path = String(url).replace("/test-token/api", "");
    if (path.endsWith("/launch")) {
      return Promise.resolve(fakeResponse({ mode: "copy", command: "resume command" }));
    }
    throw new Error(`unexpected fetch ${path}`);
  };

  for (const test of [
    {
      agent: "codex",
      bypassLabel: "Resume with YOLO",
      absentLabel: "Resume with dangerously skipped permissions"
    },
    {
      agent: "claude",
      bypassLabel: "Resume with dangerously skipped permissions",
      absentLabel: "Resume with YOLO"
    }
  ]) {
    const sourceSession = { ...session("resume", "current"), agent: test.agent };
    const actions = evaluate(environment, `actionButtons(${JSON.stringify(sourceSession)})`);
    const normal = childByText(actions, "Resume normally");
    const bypass = childByText(actions, test.bypassLabel);
    assert.ok(normal, `${test.agent} normal resume missing`);
    assert.ok(bypass, `${test.agent} bypass resume missing`);
    assert.equal(childByText(actions, test.absentLabel), undefined);

    await clickNode(normal);
    await clickNode(bypass);
    const recent = environment.requests.slice(-2).map((request) => JSON.parse(request.options.body));
    assert.deepEqual(recent, [
      { launcher: "auto", permissions: "normal" },
      { launcher: "auto", permissions: "bypass" }
    ]);
  }
}
```

Add `await testResumePermissionActions();` to `main()`.

Also add embedded-asset assertions in `TestWebApplicationIncludesCoreWorkflows`
for the two labels, the `permissions` payload, and `.resume-bypass`.

- [ ] **Step 2: Run the browser test and confirm the red state**

Run:

```sh
go test ./internal/httpapi -run '^(TestBrowserRefreshBehavior|TestWebApplicationIncludesCoreWorkflows)$' -count=1
```

Expected: FAIL because only one `Resume` button exists and the request has no
permission value.

- [ ] **Step 3: Render only the two actions valid for the source agent**

Replace the single Resume button in `actionButtons`:

```js
if (session.agent === "codex" || session.agent === "claude") {
  const resume = element("button", "", "Resume normally");
  resume.type = "button";
  resume.addEventListener("click", () => resumeSession(session.id, "normal"));

  const bypassLabel = session.agent === "codex"
    ? "Resume with YOLO"
    : "Resume with dangerously skipped permissions";
  const bypass = element("button", "resume-bypass", bypassLabel);
  bypass.type = "button";
  bypass.addEventListener("click", () => resumeSession(session.id, "bypass"));

  actions.append(resume, bypass);
}
```

Keep Analyze, optional Retitle, and Rescan before the resume controls. Keep
Delete analysis after them. Change the request function:

```js
async function resumeSession(id, permissions) {
  try {
    const result = await request("/sessions/" + encodeURIComponent(id) + "/launch", {
      method: "POST",
      body: JSON.stringify({ launcher: "auto", permissions })
    });
    if (result.command) showCommand(result.command);
    else toast("Session launched in " + (result.workspace || "cmux"));
  } catch (error) { toast(error.message, true); }
}
```

- [ ] **Step 4: Add a stable wrapping rule for the long Claude label**

Add:

```css
.detail-actions .resume-bypass {
  max-width: 220px;
  min-height: 34px;
  height: auto;
  padding-top: 7px;
  padding-bottom: 7px;
  line-height: 1.25;
  white-space: normal;
}
```

This lets the Claude action wrap without changing the global button rules or
forcing a narrow viewport wider than its content.

- [ ] **Step 5: Run focused and package browser tests**

Run:

```sh
go test ./internal/httpapi -run '^(TestBrowserRefreshBehavior|TestWebApplicationIncludesCoreWorkflows)$' -count=1
go test ./internal/httpapi -count=1
```

Expected: PASS and `browser behavior assertions passed`.

- [ ] **Step 6: Commit the web unit**

```sh
git add \
  internal/httpapi/assets/app.js \
  internal/httpapi/assets/app.css \
  internal/httpapi/testdata/app_behavior_test.js \
  internal/httpapi/api_test.go
git commit -m "feat: add agent-specific resume actions"
```

---

### Task 4: Shipped Documentation And Full Verification

**Files:**
- Modify: `README.md`
- Modify: `docs/DESIGN.md`
- Modify: `docs/IMPLEMENTATION_PLAN.md`
- Modify: `docs/superpowers/specs/2026-07-16-resume-permission-modes-design.md`

**Interfaces:**
- Consumes: the implemented API and UI behavior from Tasks 1-3.
- Produces: public safety guidance and product documents that match the shipped
  commands.

- [ ] **Step 1: Update the product design**

In `docs/DESIGN.md`, replace the single Resume action description with:

```markdown
- **Resume normally**: launch the original session with the source CLI's normal
  permission behavior or provide a copyable command.
- **Resume with bypass**: explicitly launch Codex with
  `--dangerously-bypass-approvals-and-sandbox` or Claude with
  `--dangerously-skip-permissions`.
```

In the session resume section, list all four exact commands and state that the
launch endpoint accepts only launcher and permission enums. State that missing
`permissions` defaults to `normal` and that the normal `ResumeSpec` is validated
before the launcher inserts its fixed bypass flag.

- [ ] **Step 2: Update the implementation plan and README**

In `docs/IMPLEMENTATION_PLAN.md`, update Phase 7 and Phase 8 to require both
agent-specific actions, fixed permission enum validation, exact argv tests, and
normal plus bypass manual copy verification.

In `README.md`, replace the single Resume bullet with normal and bypass actions.
Add these exact commands beneath the existing trusted-resume explanation:

```text
codex resume --dangerously-bypass-approvals-and-sandbox <session-id>
claude --dangerously-skip-permissions --resume <session-id>
```

State plainly that these buttons disable the corresponding vendor safeguards
for the resumed process, must be chosen explicitly for each launch, and do not
allow browser-supplied flags.

- [ ] **Step 3: Mark the feature specification implemented**

Change the specification status to:

```markdown
**Status:** Implemented
```

- [ ] **Step 4: Run documentation and diff checks**

Run:

```sh
rg -n "Resume normally|Resume with YOLO|dangerously-bypass|dangerously-skip|permissions.*normal" \
  README.md docs/DESIGN.md docs/IMPLEMENTATION_PLAN.md \
  docs/superpowers/specs/2026-07-16-resume-permission-modes-design.md
git diff --check
```

Expected: all four documents describe the normal and bypass behavior, and
`git diff --check` prints nothing.

- [ ] **Step 5: Run the complete automated verification suite**

Run:

```sh
gofmt -w internal/launch/launch.go internal/launch/launch_test.go \
  internal/httpapi/api.go internal/httpapi/api_test.go
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build ./cmd/agent-history
```

Expected: every command exits zero. No test invokes a real analyzer or resumes a
real session.

- [ ] **Step 6: Verify the embedded UI at desktop and narrow widths**

Start a disposable server or rebuild/restart the existing loopback service.
Open the UI in a new browser tab. Inspect one Codex session and one Claude
session at desktop width and at approximately 390 CSS pixels:

- Codex shows **Resume normally** and **Resume with YOLO** only.
- Claude shows **Resume normally** and **Resume with dangerously skipped
  permissions** only.
- The long Claude label wraps cleanly with no overlap or horizontal overflow.
- Do not click either action against a real session.

Check the browser console for errors and capture desktop and narrow screenshots
for verification.

- [ ] **Step 7: Commit documentation**

```sh
git add README.md docs/DESIGN.md docs/IMPLEMENTATION_PLAN.md \
  docs/superpowers/specs/2026-07-16-resume-permission-modes-design.md
git commit -m "docs: describe resume permission choices"
```

- [ ] **Step 8: Review the final branch**

Run:

```sh
git status --short --branch
git log --oneline --decorate -8
git diff origin/main...HEAD --stat
git diff --check origin/main...HEAD
```

Expected: a clean worktree, four focused implementation commits after the
design/plan commits, and no whitespace errors.
