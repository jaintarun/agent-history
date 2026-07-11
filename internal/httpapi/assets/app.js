"use strict";

const basePath = location.pathname.endsWith("/") ? location.pathname : location.pathname + "/";
const apiPath = basePath + "api";
const state = { sessions: [], nextCursor: "", selectedID: "", searchAbort: null, pollTimer: null };
const elements = Object.fromEntries(Array.from(document.querySelectorAll("[id]")).map((element) => [element.id, element]));
const filterIDs = [
  "session-search", "agent-filter", "active-filter", "cwd-filter", "topic-filter", "status-filter", "sort-filter",
  "active-after-filter", "active-before-filter", "started-after-filter", "started-before-filter"
];
const filterParams = {
  "session-search": "q", "agent-filter": "agent", "active-filter": "active", "cwd-filter": "cwd",
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

function setURLFromFilters() {
  const params = new URLSearchParams();
  for (const id of filterIDs) {
    const value = elements[id].value.trim();
    if (value && !(id === "sort-filter" && value === "last_active")) params.set(filterParams[id], value);
  }
  if (state.selectedID) params.set("session", state.selectedID);
  history.replaceState(null, "", basePath + (params.size ? "?" + params.toString() : ""));
  renderFilterChips();
}

function restoreFilters() {
  const params = new URLSearchParams(location.search);
  for (const id of filterIDs) elements[id].value = params.get(filterParams[id]) || (id === "sort-filter" ? "last_active" : "");
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
    ["session-search", "q"], ["agent-filter", "agent"], ["cwd-filter", "cwd"],
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
  if (cursor) params.set("cursor", cursor);
  params.set("limit", "50");
  return params;
}

function renderFilterChips() {
  elements["filter-chips"].replaceChildren();
  const labels = {
    q: "Search", agent: "Agent", active: "Active", cwd: "Folder",
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

function clearFilters() {
  for (const id of filterIDs) elements[id].value = id === "sort-filter" ? "last_active" : "";
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
  state.selectedID = id;
  if (updateURL) setURLFromFilters();
  for (const row of elements["session-results"].children) row.setAttribute("aria-selected", String(row.dataset.sessionId === id));
  elements["detail-empty"].hidden = true;
  elements["detail-content"].hidden = false;
  elements["detail-content"].replaceChildren(element("div", "state-line", "Loading session..."));
  clearTimeout(state.pollTimer);
  try {
    const [session, messageData] = await Promise.all([
      request("/sessions/" + encodeURIComponent(id)),
      request("/sessions/" + encodeURIComponent(id) + "/messages?include_tools=true")
    ]);
    renderDetail(session, messageData.messages);
    if (session.analysis_status === "queued" || session.analysis_status === "running") {
      state.pollTimer = setTimeout(() => { selectSession(id, false); loadSessions(false); }, 1800);
    }
  } catch (error) {
    elements["detail-content"].replaceChildren(element("div", "state-line", error.message));
  }
}

function renderDetail(session, messages) {
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

  const conversation = element("section", "detail-section");
  const conversationHeading = element("div", "section-heading");
  conversationHeading.append(element("h2", "", `Visible conversation (${messages.length})`));
  const toggleLabel = element("label", "check-row");
  const toggle = document.createElement("input");
  toggle.type = "checkbox";
  toggle.checked = true;
  toggleLabel.append(toggle, document.createTextNode("Show tools"));
  conversationHeading.append(toggleLabel);
  const messageList = element("div", "messages");
  renderMessages(messageList, messages, true);
  toggle.addEventListener("change", () => renderMessages(messageList, messages, toggle.checked));
  conversation.append(conversationHeading, messageList);
  content.append(conversation);
}

function actionButtons(session) {
  const actions = element("div", "detail-actions");
  const analyze = element("button", "", session.analysis_status === "none" ? "Analyze" : "Reanalyze");
  analyze.type = "button";
  analyze.disabled = session.analysis_status === "queued" || session.analysis_status === "running";
  analyze.addEventListener("click", () => mutateSession(session.id, "/analyze", "POST", { full: session.analysis_status !== "none" }, "Analysis queued"));
  const rescan = element("button", "", "Rescan");
  rescan.type = "button";
  rescan.addEventListener("click", () => mutateSession(session.id, "/rescan", "POST", {}, "Session rescanned"));
  const resume = element("button", "", "Resume");
  resume.type = "button";
  resume.addEventListener("click", () => resumeSession(session.id));
  actions.append(analyze, rescan, resume);
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

function renderMessages(container, messages, includeTools) {
  container.replaceChildren();
  for (const message of messages) {
    if (!includeTools && message.role === "tool") continue;
    const row = element("article", "message");
    const role = element("div", "message-role", message.tool_name || message.role);
    role.append(element("div", "", shortDate(message.timestamp)));
    row.append(role, element("pre", "message-text", message.text));
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

async function resumeSession(id) {
  try {
    const result = await request("/sessions/" + encodeURIComponent(id) + "/launch", { method: "POST", body: JSON.stringify({ launcher: "auto" }) });
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

async function openSettings() {
  try {
    const settings = await request("/settings");
    elements["settings-provider"].value = settings.analysis_provider;
    elements["settings-model"].value = settings.analysis_model;
    elements["settings-auto"].checked = settings.analysis_auto;
    elements["settings-error"].textContent = "";
    elements["settings-dialog"].showModal();
  } catch (error) { toast(error.message, true); }
}

function statusText(status) {
  return { partial: "New activity", queued: "Queued", running: "Running", failed: "Failed" }[status] || status;
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
elements["load-more"].addEventListener("click", () => loadSessions(true));
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
elements["settings-button"].addEventListener("click", openSettings);
elements["settings-cancel"].addEventListener("click", () => elements["settings-dialog"].close());
elements["settings-form"].addEventListener("submit", async (event) => {
  event.preventDefault();
  try {
    await request("/settings", { method: "PUT", body: JSON.stringify({
      analysis_provider: elements["settings-provider"].value,
      analysis_model: elements["settings-model"].value,
      analysis_auto: elements["settings-auto"].checked
    }) });
    elements["settings-dialog"].close();
    toast("Analysis settings saved");
  } catch (error) { elements["settings-error"].textContent = error.message; }
});

restoreFilters();
renderFilterChips();
loadFacets();
loadSessions(false);
