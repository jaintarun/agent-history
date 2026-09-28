"use strict";

const basePath = location.pathname.endsWith("/") ? location.pathname : location.pathname + "/";
const apiPath = basePath + "api";
const autoRefreshStorageKey = "agent-history.auto-refresh";

function loadAutoRefreshPreference() {
  try { return localStorage.getItem(autoRefreshStorageKey) !== "off"; }
  catch { return true; }
}

function saveAutoRefreshPreference(enabled) {
  try { localStorage.setItem(autoRefreshStorageKey, enabled ? "on" : "off"); }
  catch { /* Browser storage is optional. */ }
}

const state = {
  sessions: [], nextCursor: "", selectedID: "", searchAbort: null,
  pollTimer: null, cmuxStatus: null,
  autoRefresh: loadAutoRefreshPreference(), selectedAnalysisStatus: "", detailRevision: 0,
  settingsProvider: "", settingsProviders: []
};
const refreshIntervalSeconds = 60;
let refreshSeconds = refreshIntervalSeconds;
let refreshTimer;
const elements = Object.fromEntries(Array.from(document.querySelectorAll("[id]")).map((element) => [element.id, element]));
const filterIDs = [
  "session-search", "agent-filter", "active-filter", "cmux-filter", "cwd-filter", "topic-filter", "status-filter", "sort-filter",
  "active-after-filter", "active-before-filter", "started-after-filter", "started-before-filter"
];
const filterParams = {
  "session-search": "q", "agent-filter": "agent", "active-filter": "active", "cmux-filter": "cmux", "cwd-filter": "cwd",
  "topic-filter": "topic_mode", "status-filter": "analysis_status", "sort-filter": "sort",
  "active-after-filter": "active_after", "active-before-filter": "active_before",
  "started-after-filter": "started_after", "started-before-filter": "started_before"
};

async function request(path, options = {}) {
  const response = await fetch(apiPath + path, {
    ...options,
    headers: { ...(options.body ? { "Content-Type": "application/json" } : {}), ...(options.headers || {}) }
  });
  if (response.status === 204) return null;
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error?.message || `Request failed (${response.status})`);
  return body;
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

const actionHelpText = {
  refresh: "Reloads sessions, selected-session details, settings, and cmux status. It does not scan transcript files or run AI.",
  retitleWeak: "Uses AI to replace short, generic, or duplicate titles. Session summaries and topics stay unchanged.",
  scan: "Checks all Codex, Claude, and Grok transcript files and imports new or changed sessions. It does not itself run AI analysis.",
  analyze: "Uses the selected AI provider to create this session's title, summary, and topics.",
  reanalyze: "Uses the selected AI provider to replace this session's title, summary, and topics. Existing analysis stays if it fails.",
  retitle: "Uses existing topic summaries to generate only a new title. It does not reread the full conversation.",
  rescan: "Rereads this session's source transcript and imports changes. It does not run AI analysis."
};
let actionHelpSequence = 0;

function explainAction(button, label, description) {
  button.className = `${button.className || ""} has-action-help`.trim();
  button.setAttribute("aria-label", label);
  const help = element("span", "action-help");
  const icon = element("span", "action-help-icon", "i");
  icon.setAttribute("aria-hidden", "true");
  const tooltip = element("span", "action-tooltip", description);
  tooltip.id = `action-help-${button.id || ++actionHelpSequence}`;
  tooltip.setAttribute("role", "tooltip");
  button.setAttribute("aria-describedby", tooltip.id);
  help.addEventListener("click", (event) => {
    event.preventDefault();
    event.stopPropagation();
  });
  help.append(icon, tooltip);
  button.append(help);
  return button;
}

explainAction(elements["refresh-button"], "Refresh", actionHelpText.refresh);
explainAction(elements["retitle-weak-button"], "Retitle weak titles", actionHelpText.retitleWeak);
explainAction(elements["scan-button"], "Scan", actionHelpText.scan);

function setURLFromFilters() {
  const params = new URLSearchParams();
  for (const id of filterIDs) {
    const value = elements[id].value.trim();
    if (value && !(id === "sort-filter" && value === "last_active")) params.set(filterParams[id], value);
  }
  if (elements["include-messages-filter"].checked) params.set("include_messages", "true");
  if (state.selectedID) params.set("session", state.selectedID);
  history.replaceState(null, "", basePath + (params.size ? "?" + params.toString() : ""));
  renderFilterChips();
}

function restoreFilters() {
  const params = new URLSearchParams(location.search);
  for (const id of filterIDs) elements[id].value = params.get(filterParams[id]) || (id === "sort-filter" ? "last_active" : "");
  elements["include-messages-filter"].checked = params.get("include_messages") === "true";
  updateSearchPlaceholder();
  state.selectedID = params.get("session") || "";
}

function activeAfter(value) {
  if (!value) return "";
  const date = new Date();
  const days = { "7d": 7, "30d": 30, "3m": 92, "6m": 183, "1y": 365 }[value];
  date.setUTCDate(date.getUTCDate() - days);
  return date.toISOString();
}

function searchParams(cursor = "") {
  const params = new URLSearchParams();
  const mappings = [
    ["session-search", "q"], ["agent-filter", "agent"], ["cmux-filter", "cmux"], ["cwd-filter", "cwd"],
    ["topic-filter", "topic_mode"], ["status-filter", "analysis_status"], ["sort-filter", "sort"],
    ["active-after-filter", "active_after"], ["active-before-filter", "active_before"],
    ["started-after-filter", "started_after"], ["started-before-filter", "started_before"]
  ];
  for (const [id, name] of mappings) {
    const value = elements[id].value.trim();
    if (!value) continue;
    if (name.endsWith("_after")) params.set(name, new Date(value + "T00:00:00").toISOString());
    else if (name.endsWith("_before")) params.set(name, new Date(value + "T23:59:59").toISOString());
    else params.set(name, value);
  }
  const after = activeAfter(elements["active-filter"].value);
  if (after) params.set("active_after", after);
  if (elements["include-messages-filter"].checked) params.set("include_messages", "true");
  if (cursor) params.set("cursor", cursor);
  params.set("limit", "50");
  return params;
}

function renderFilterChips() {
  elements["filter-chips"].replaceChildren();
  const labels = {
    q: "Search", agent: "Agent", active: "Active", cmux: "cmux", cwd: "Folder",
    topic_mode: "Topics", analysis_status: "Analysis", sort: "Sort", active_after: "Active from",
    active_before: "Active through", started_after: "Started from", started_before: "Started through"
  };
  for (const id of filterIDs) {
    const value = elements[id].value.trim();
    if (!value || (id === "sort-filter" && value === "last_active")) continue;
    const chipLabel = `${labels[filterParams[id]]}: ${selectedLabel(elements[id])}`;
    const button = element("button", "filter-chip", chipLabel + "  \u00d7");
    button.type = "button";
    button.setAttribute("aria-label", `Remove ${chipLabel} filter`);
    button.addEventListener("click", () => {
      elements[id].value = id === "sort-filter" ? "last_active" : "";
      applyFilters();
      elements[id].focus();
    });
    elements["filter-chips"].append(button);
  }
  if (elements["filter-chips"].children.length > 1) {
    const clear = element("button", "filter-chip", "Clear all");
    clear.type = "button";
    clear.addEventListener("click", clearFilters);
    elements["filter-chips"].append(clear);
  }
}

function selectedLabel(control) {
  if (control.tagName === "SELECT") return control.selectedOptions[0]?.textContent || control.value;
  return control.value;
}

function updateSearchPlaceholder() {
  elements["session-search"].placeholder = elements["include-messages-filter"].checked
    ? "Search summaries and conversations"
    : "Search summaries and topics";
}

function clearFilters() {
  for (const id of filterIDs) elements[id].value = id === "sort-filter" ? "last_active" : "";
  elements["include-messages-filter"].checked = false;
  updateSearchPlaceholder();
  applyFilters();
}

let searchDebounce;
function applyFilters() {
  clearTimeout(searchDebounce);
  setURLFromFilters();
  searchDebounce = setTimeout(() => loadSessions(false), 220);
}

async function loadSessions(append) {
  if (state.searchAbort) state.searchAbort.abort();
  state.searchAbort = new AbortController();
  if (!append) {
    state.sessions = [];
    state.nextCursor = "";
    elements["results-status"].textContent = "Loading sessions...";
    elements["session-results"].replaceChildren();
  }
  try {
    const data = await request("/sessions?" + searchParams(append ? state.nextCursor : ""), { signal: state.searchAbort.signal });
    state.sessions.push(...data.sessions);
    state.nextCursor = data.next_cursor || "";
    renderSessions();
    if (!append && state.selectedID) await selectSession(state.selectedID, false);
  } catch (error) {
    if (error.name !== "AbortError") elements["results-status"].textContent = error.message;
  }
}

function updateRefreshButton() {
  elements["refresh-label"].textContent = "Refresh";
  elements["refresh-countdown"].textContent = `in ${refreshSeconds}s`;
}

function stopScheduledRefreshes() {
  clearInterval(refreshTimer);
  clearTimeout(state.pollTimer);
  state.pollTimer = null;
  elements["refresh-label"].textContent = "Refresh";
  elements["refresh-countdown"].textContent = "";
}

function scheduleSelectedSessionPoll() {
  clearTimeout(state.pollTimer);
  state.pollTimer = null;
  if (!state.autoRefresh || !state.selectedID ||
      (state.selectedAnalysisStatus !== "queued" && state.selectedAnalysisStatus !== "running")) return;
  const id = state.selectedID;
  state.pollTimer = setTimeout(() => {
    if (state.selectedID === id) {
      void selectSession(id, false);
      void loadSessions(false);
    }
  }, 1800);
}

function startRefreshCountdown() {
  clearInterval(refreshTimer);
  if (!state.autoRefresh) {
    stopScheduledRefreshes();
    return;
  }
  refreshSeconds = refreshIntervalSeconds;
  updateRefreshButton();
  refreshTimer = setInterval(() => {
    refreshSeconds -= 1;
    if (refreshSeconds <= 0) {
      void refreshPageData(false);
      return;
    }
    updateRefreshButton();
  }, 1000);
}

async function refreshPageData(showCmuxError) {
  clearInterval(refreshTimer);
  elements["refresh-button"].disabled = true;
  elements["refresh-label"].textContent = "Refreshing...";
  elements["refresh-countdown"].textContent = "";
  let cmuxError;
  try {
    try {
      await request("/cmux/refresh", { method: "POST", body: JSON.stringify({}) });
    } catch (error) {
      cmuxError = error;
    }
    await loadCmuxStatus();
    await loadSessions(false);
    if (cmuxError && showCmuxError) toast(cmuxError.message, true);
  } finally {
    elements["refresh-button"].disabled = false;
    startRefreshCountdown();
  }
}

function renderSessions() {
  elements["session-results"].replaceChildren();
  elements["results-status"].textContent = state.sessions.length ? "" : "No sessions match these filters.";
  elements["result-count"].textContent = `${state.sessions.length}${state.nextCursor ? "+" : ""} shown`;
  elements["load-more"].hidden = !state.nextCursor;
  for (const session of state.sessions) {
    const row = element("button", "session-row");
    row.type = "button";
    row.setAttribute("role", "option");
    row.setAttribute("aria-selected", String(session.id === state.selectedID));
    row.dataset.sessionId = session.id;
    const top = element("div", "row-top");
    top.append(element("span", "agent-label", session.agent));
    if (session.cmux?.open) top.append(element("span", `cmux-label ${session.cmux.lifecycle}`, `cmux: ${cmuxLifecycleText(session.cmux.lifecycle)}`));
    if (session.topic_count !== null) top.append(element("span", "topic-label", session.multiple_topics ? `${session.topic_count} topics` : "Focused"));
    if (session.analysis_status !== "current" && session.analysis_status !== "none") top.append(element("span", `status-label ${session.analysis_status}`, statusText(session.analysis_status)));
    row.append(top);
    row.append(element("div", "row-title", session.title || firstLine(session.snippet) || "Unanalyzed session"));
    row.append(element("div", "row-summary", session.summary || session.snippet || "No generated summary yet."));
    const meta = element("div", "row-meta");
    meta.append(element("span", "row-path", session.working_directory || "Unknown folder"));
    meta.append(element("span", "row-time", relativeTime(session.last_active_at)));
    row.append(meta);
    row.addEventListener("click", () => selectSession(session.id, true));
    elements["session-results"].append(row);
  }
}

async function selectSession(id, updateURL) {
  const revision = ++state.detailRevision;
  state.selectedID = id;
  state.selectedAnalysisStatus = "";
  if (updateURL) setURLFromFilters();
  for (const row of elements["session-results"].children) row.setAttribute("aria-selected", String(row.dataset.sessionId === id));
  elements["detail-empty"].hidden = true;
  elements["detail-content"].hidden = false;
  elements["detail-content"].replaceChildren(element("div", "state-line", "Loading session..."));
  clearTimeout(state.pollTimer);
  try {
    const session = await request("/sessions/" + encodeURIComponent(id));
    if (state.detailRevision !== revision) return;
    renderDetail(session, revision);
    state.selectedAnalysisStatus = session.analysis_status;
    scheduleSelectedSessionPoll();
  } catch (error) {
    if (state.detailRevision !== revision) return;
    elements["detail-content"].replaceChildren(element("div", "state-line", error.message));
  }
}

function renderDetail(session, revision) {
  const content = elements["detail-content"];
  content.replaceChildren();
  const header = element("header", "detail-header");
  const titleLine = element("div", "detail-title-line");
  const titleWrap = element("div");
  const title = element("h2", "detail-title", session.title || "Unanalyzed session");
  titleWrap.append(title, element("div", "muted", session.working_directory || "Unknown working folder"));
  titleLine.append(titleWrap, actionButtons(session));
  header.append(titleLine, metadata(session));
  content.append(header);
  content.append(renderCmuxComparison(session));

  const summary = element("section", "summary-band");
  summary.append(element("h2", "", "Session overview"), element("p", "", session.summary || "Analyze this session to generate a specific title, overview, and chronological topic chapters."));
  if (session.analysis_status === "partial") {
    const through = session.analyzed_through_sequence === undefined ? "the previous summary" : `message ${session.analyzed_through_sequence}${session.analyzed_through_at ? ` on ${formatDate(session.analyzed_through_at)}` : ""}`;
    summary.append(element("div", "analysis-notice", `Analysis covers through ${through}. New conversation exists beyond it; analyze again to roll it into the summary.`));
  }
  if (session.analysis_status === "queued" || session.analysis_status === "running") summary.append(element("div", "analysis-notice", `Analysis is ${session.analysis_status}. This view will refresh automatically.`));
  if (session.analysis_status === "failed") summary.append(element("div", "analysis-notice error-notice", session.analysis_error || "Analysis failed. The previous successful summary remains available."));
  content.append(summary);

  const topics = element("section", "detail-section");
  const topicsHeading = element("div", "section-heading");
  topicsHeading.append(element("h2", "", `Topics${session.topic_count !== null ? ` (${session.topic_count})` : ""}`));
  topics.append(topicsHeading);
  if (session.topics?.length) session.topics.forEach((topic, index) => topics.append(topicNode(topic, index)));
  else topics.append(element("p", "muted", "No topic chapters yet."));
  content.append(topics);

  const conversation = element("section", "detail-section conversation-section");
  const conversationHeading = element("h2", "section-heading", "Conversation");
  const messageList = element("div", "messages");
  messageList.replaceChildren(element("div", "state-line", "Loading conversation..."));
  conversation.append(conversationHeading, messageList);
  content.append(conversation);
  request("/sessions/" + encodeURIComponent(session.id) + "/messages?include_tools=false")
    .then((data) => {
      if (state.detailRevision !== revision) return;
      renderMessages(messageList, data.messages);
      if (!data.messages.length) messageList.replaceChildren(element("p", "muted", "No visible conversation."));
      conversationHeading.textContent = `Conversation (${data.messages.length})`;
    })
    .catch((error) => {
      if (state.detailRevision === revision) messageList.replaceChildren(element("div", "state-line", error.message));
    });
}

function renderCmuxComparison(session) {
  const band = element("section", "cmux-band");
  const heading = element("div", "cmux-heading");
  heading.append(element("h2", "", "cmux"));
  const status = state.cmuxStatus;
  if (status && !status.available) {
    heading.append(element("span", "cmux-state unavailable", "Unavailable"));
    band.append(heading, element("p", "muted", status.error || "cmux is unavailable."));
    return band;
  }
  if (!session.cmux?.open) {
    heading.append(element("span", "cmux-state", "Not open"));
    band.append(heading, element("p", "muted", "Not open in cmux"));
    return band;
  }

  let metadataState = session.cmux.title_state;
  if (session.cmux.description_state === "different") metadataState = "different";
  else if (metadataState === "synced" && session.cmux.description_state === "no_agent_description") metadataState = "no_agent_description";
  const metadataStateText = {
    synced: "Synced",
    different: "Different - cmux metadata preserved",
    no_agent_title: "No Agent History title",
    no_agent_description: "No Agent History description"
  }[metadataState] || metadataState;
  heading.append(element("span", `cmux-state ${metadataState}`, metadataStateText));
  if ((session.cmux.title_state === "different" || session.cmux.description_state === "different") && session.title) {
    const push = element("button", "", "Send title and description to cmux");
    push.type = "button";
    push.addEventListener("click", () => mutateSession(session.id, "/cmux-title", "POST", {}, "Title and description sent to cmux"));
    heading.append(push);
  }
  band.append(heading);
  const comparison = element("div", "cmux-comparison");
  const target = session.cmux.target === "tab" ? "cmux tab title" : "cmux workspace title";
  comparison.append(cmuxTitleValue(target, session.cmux.target_title || "Untitled"));
  comparison.append(cmuxTitleValue("Agent History title", session.title || "No generated title"));
  if (session.cmux.target === "workspace") {
    comparison.append(cmuxTitleValue("cmux workspace description", session.cmux.workspace_description || "No description"));
    comparison.append(cmuxTitleValue("Agent History description", session.summary || "No generated description"));
  }
  band.append(comparison);
  if (session.cmux.target === "workspace" && session.cmux.surface_title && session.cmux.surface_title.trim() !== session.cmux.target_title.trim()) {
    band.append(element("p", "cmux-secondary", `Mapped tab: ${session.cmux.surface_title}`));
  }
  band.append(element("p", "cmux-secondary", `${cmuxLifecycleText(session.cmux.lifecycle)} in cmux`));
  return band;
}

function cmuxTitleValue(label, value) {
  const item = element("div", "cmux-title-value");
  item.append(element("span", "metadata-label", label), element("span", "", value));
  return item;
}

function actionButtons(session) {
  const actions = element("div", "detail-actions");
  const analyzeLabel = session.analysis_status === "none" ? "Analyze" : "Reanalyze";
  const analyze = element("button", "", analyzeLabel);
  analyze.type = "button";
  analyze.disabled = session.analysis_status === "queued" || session.analysis_status === "running";
  analyze.addEventListener("click", () => mutateSession(session.id, "/analyze", "POST", { full: session.analysis_status !== "none" }, "Analysis queued"));
  const retitle = element("button", "", "Retitle");
  retitle.type = "button";
  retitle.disabled = session.analysis_status === "queued" || session.analysis_status === "running";
  retitle.addEventListener("click", () => mutateSession(session.id, "/retitle", "POST", {}, "Retitle queued"));
  const rescan = element("button", "", "Rescan");
  rescan.type = "button";
  rescan.addEventListener("click", () => mutateSession(session.id, "/rescan", "POST", {}, "Session rescanned"));
  actions.append(explainAction(analyze, analyzeLabel, session.analysis_status === "none" ? actionHelpText.analyze : actionHelpText.reanalyze));
  if (session.topics?.length) actions.append(explainAction(retitle, "Retitle", actionHelpText.retitle));
  actions.append(explainAction(rescan, "Rescan", actionHelpText.rescan));
  if (session.agent === "codex" || session.agent === "claude" || session.agent === "grok") {
    const resume = element("button", "", "Resume normally");
    resume.type = "button";
    resume.addEventListener("click", () => resumeSession(session.id, "normal"));
    const bypassLabel = session.agent === "claude"
      ? "Resume with dangerously skipped permissions"
      : "Resume with YOLO";
    const bypass = element("button", "resume-bypass", bypassLabel);
    bypass.type = "button";
    bypass.addEventListener("click", () => resumeSession(session.id, "bypass"));
    actions.append(resume, bypass);
  }
  if (session.analysis_status !== "none" || session.title) {
    const remove = element("button", "danger", "Delete analysis");
    remove.type = "button";
    remove.addEventListener("click", () => {
      elements["confirm-dialog"].returnValue = "";
      elements["confirm-dialog"].showModal();
    });
    actions.append(remove);
  }
  return actions;
}

function metadata(session) {
  const grid = element("div", "metadata-grid");
  const values = [
    ["Agent", session.agent], ["Started", formatDate(session.started_at)],
    ["Last active", formatDate(session.last_active_at)], ["Session span", duration(session.span_seconds)],
    ["Native session", session.native_session_id], ["Source file", session.source_path]
  ];
  for (const [label, value] of values) {
    const item = element("div", "metadata-item");
    item.append(element("span", "metadata-label", label), element("span", "metadata-value", value));
    grid.append(item);
  }
  return grid;
}

function topicNode(topic, index) {
  const details = element("details", "topic");
  if (index === 0) details.open = true;
  const summary = document.createElement("summary");
  const copy = element("div");
  copy.append(element("div", "topic-title", topic.title), element("div", "topic-summary", topic.summary));
  summary.append(element("span", "topic-index", String(index + 1)), copy);
  details.append(summary, element("div", "topic-detail", topic.detail));
  return details;
}

function renderMessages(container, messages) {
  container.replaceChildren();
  for (const message of messages) {
    const row = element("article", "message");
    const role = element("div", "message-role", message.tool_name || message.role);
    role.append(element("div", "", shortDate(message.timestamp)));
    const body = element("div", "message-text");
    if (message.html) body.innerHTML = message.html;
    else body.textContent = message.text;
    row.append(role, body);
    container.append(row);
  }
}

async function mutateSession(id, suffix, method, body, success) {
  try {
    await request("/sessions/" + encodeURIComponent(id) + suffix, { method, body: JSON.stringify(body) });
    toast(success);
    await selectSession(id, false);
    await loadSessions(false);
  } catch (error) { toast(error.message, true); }
}

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

function showCommand(command) {
  elements["resume-command"].value = command;
  elements["copy-status"].textContent = "";
  elements["command-dialog"].showModal();
}

async function loadFacets() {
  try {
    const facets = await request("/sessions/facets");
    elements["directory-list"].replaceChildren();
    for (const directory of facets.directories || []) {
      const option = document.createElement("option");
      option.value = directory.Value || directory.value;
      elements["directory-list"].append(option);
    }
  } catch (error) { toast(error.message, true); }
}

function updateCmuxStatus(settings) {
  state.cmuxStatus = {
    available: settings.cmux_available,
    accessMode: settings.cmux_access_mode,
    error: settings.cmux_error,
    observedAt: settings.cmux_observed_at
  };
}

async function loadCmuxStatus() {
  const settings = await request("/settings");
  updateCmuxStatus(settings);
  return settings;
}

function settingsProvider(id) {
  return state.settingsProviders.find((provider) => provider.id === id);
}

function updateSettingsProviderStatus() {
  const provider = settingsProvider(elements["settings-provider"].value);
  if (!provider) {
    elements["settings-provider-status"].textContent = "";
    return;
  }
  elements["settings-provider-status"].textContent = provider.available
    ? `Uses existing ${provider.name} CLI authentication.`
    : `Install ${provider.name}, then restart Agent History.`;
}

function renderSettingsProviders(settings) {
  state.settingsProviders = settings.analysis_providers || [];
  state.settingsProvider = settings.analysis_provider;
  elements["settings-provider"].replaceChildren();
  for (const provider of state.settingsProviders) {
    const option = document.createElement("option");
    option.value = provider.id;
    option.textContent = provider.name + (provider.available ? "" : " (Not installed)");
    option.disabled = !provider.available && provider.id !== settings.analysis_provider;
    elements["settings-provider"].append(option);
  }
  elements["settings-provider"].value = settings.analysis_provider;
  updateSettingsProviderStatus();
}

async function openSettings() {
  try {
    const settings = await loadCmuxStatus();
    renderSettingsProviders(settings);
    elements["settings-model"].value = settings.analysis_model;
    elements["settings-auto"].checked = settings.analysis_auto;
    elements["settings-cmux-sync"].checked = settings.cmux_title_sync;
    elements["settings-cmux-status"].textContent = settings.cmux_available
      ? `cmux connected (${settings.cmux_access_mode})`
      : (settings.cmux_error || "cmux is unavailable");
    elements["settings-error"].textContent = "";
    elements["settings-dialog"].showModal();
  } catch (error) { toast(error.message, true); }
}

function statusText(status) {
  return { partial: "New activity", queued: "Queued", running: "Running", failed: "Failed" }[status] || status;
}
function cmuxLifecycleText(lifecycle) {
  return { running: "Running", idle: "Idle", needsInput: "Needs input", unknown: "Unknown" }[lifecycle] || "Unknown";
}
function firstLine(value) { return (value || "").split("\n")[0].slice(0, 100); }
function formatDate(value) { return value ? new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value)) : "Unknown"; }
function shortDate(value) { return value ? new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(new Date(value)) : ""; }
function relativeTime(value) {
  if (!value) return "Unknown";
  const seconds = Math.round((new Date(value).getTime() - Date.now()) / 1000);
  const units = [[31536000, "year"], [2592000, "month"], [86400, "day"], [3600, "hour"], [60, "minute"]];
  for (const [size, unit] of units) if (Math.abs(seconds) >= size) return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(Math.round(seconds / size), unit);
  return "just now";
}
function duration(seconds) {
  const days = Math.floor(seconds / 86400), hours = Math.floor((seconds % 86400) / 3600), minutes = Math.floor((seconds % 3600) / 60);
  return [days ? `${days}d` : "", hours ? `${hours}h` : "", minutes || (!days && !hours) ? `${minutes}m` : ""].filter(Boolean).join(" ");
}
function toast(message, isError = false) {
  elements.toast.textContent = message;
  elements.toast.style.background = isError ? "#762222" : "#173f31";
  elements.toast.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { elements.toast.hidden = true; }, 4200);
}

elements.filters.addEventListener("submit", (event) => { event.preventDefault(); applyFilters(); });
for (const id of filterIDs) {
  const eventName = id === "session-search" || id === "cwd-filter" ? "input" : "change";
  elements[id].addEventListener(eventName, applyFilters);
}
elements["include-messages-filter"].addEventListener("change", () => {
  updateSearchPlaceholder();
  applyFilters();
});
elements["load-more"].addEventListener("click", () => loadSessions(true));
elements["auto-refresh-toggle"].checked = state.autoRefresh;
elements["auto-refresh-toggle"].addEventListener("change", (event) => {
  state.autoRefresh = event.currentTarget.checked;
  saveAutoRefreshPreference(state.autoRefresh);
  if (state.autoRefresh) {
    startRefreshCountdown();
    scheduleSelectedSessionPoll();
  } else {
    stopScheduledRefreshes();
  }
});
elements["refresh-button"].addEventListener("click", () => refreshPageData(true));
elements["session-results"].addEventListener("keydown", (event) => {
  if (event.key !== "ArrowDown" && event.key !== "ArrowUp" && event.key !== "Enter") return;
  const rows = Array.from(elements["session-results"].children);
  if (!rows.length) return;
  let index = rows.findIndex((row) => row.dataset.sessionId === state.selectedID);
  if (event.key === "ArrowDown") index = Math.min(rows.length - 1, index + 1);
  if (event.key === "ArrowUp") index = Math.max(0, index < 0 ? 0 : index - 1);
  if (event.key === "Enter" && index >= 0) rows[index].click();
  else if (event.key !== "Enter") { rows[index].focus(); rows[index].click(); }
  event.preventDefault();
});
elements["confirm-dialog"].addEventListener("close", () => {
  if (elements["confirm-dialog"].returnValue === "confirm" && state.selectedID) mutateSession(state.selectedID, "/analysis", "DELETE", null, "Analysis deleted");
});
elements["copy-command"].addEventListener("click", async () => {
  try { await navigator.clipboard.writeText(elements["resume-command"].value); elements["copy-status"].textContent = "Copied."; }
  catch { elements["resume-command"].select(); elements["copy-status"].textContent = "Select and copy the command."; }
});
elements["scan-button"].addEventListener("click", async () => {
  elements["scan-button"].disabled = true;
  try { await request("/scan", { method: "POST", body: JSON.stringify({ agent: "all" }) }); toast("History scan complete"); await loadSessions(false); await loadFacets(); }
  catch (error) { toast(error.message, true); }
  finally { elements["scan-button"].disabled = false; }
});
elements["retitle-weak-button"].addEventListener("click", async () => {
  if (!window.confirm("Queue title-only model calls for all weak titles?")) return;
  elements["retitle-weak-button"].disabled = true;
  try {
    const result = await request("/retitle-weak", { method: "POST", body: JSON.stringify({}) });
    toast(`${result.queued} of ${result.matched} weak titles queued`);
    await loadSessions(false);
    await loadFacets();
  } catch (error) { toast(error.message, true); }
  finally { elements["retitle-weak-button"].disabled = false; }
});
elements["settings-button"].addEventListener("click", openSettings);
elements["settings-cancel"].addEventListener("click", () => elements["settings-dialog"].close());
elements["settings-provider"].addEventListener("change", () => {
  const previous = settingsProvider(state.settingsProvider);
  const selected = settingsProvider(elements["settings-provider"].value);
  const model = elements["settings-model"].value.trim();
  if (selected && (!model || (previous && model === previous.default_model))) {
    elements["settings-model"].value = selected.default_model;
  }
  state.settingsProvider = elements["settings-provider"].value;
  updateSettingsProviderStatus();
});
elements["settings-form"].addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    const settings = await request("/settings", { method: "PUT", body: JSON.stringify({
      analysis_provider: elements["settings-provider"].value,
      analysis_model: elements["settings-model"].value,
      analysis_auto: elements["settings-auto"].checked,
      cmux_title_sync: elements["settings-cmux-sync"].checked
    }) });
    updateCmuxStatus(settings);
    elements["settings-dialog"].close();
    toast("Settings saved");
  } catch (error) { elements["settings-error"].textContent = error.message; }
});

restoreFilters();
renderFilterChips();
loadFacets();
loadCmuxStatus().then(() => loadSessions(false)).catch((error) => {
  state.cmuxStatus = { available: false, accessMode: "", error: error.message, observedAt: "" };
  loadSessions(false);
});
startRefreshCountdown();
