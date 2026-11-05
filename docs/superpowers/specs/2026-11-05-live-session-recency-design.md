# Live-session priority and recency-first metadata

## Intent

Long-running agent sessions can change subjects repeatedly. Agent History's
visible title and overview should describe the latest meaningful work first,
with earlier work condensed after it. For sessions open in cmux, newly added
conversation should reach those fields within roughly five minutes and then
flow to cmux's live title and workspace description. Agent History remains the
source of truth; this is not a separate cmux-only label.

## Goals

- Refresh already matched, open cmux agent sessions on a five-minute cadence.
- Prioritize changed live sessions over historical analysis backlog without
  starting concurrent model requests.
- Make the Agent History session title and overview recency-first, and let the
  existing cmux reconciler mirror them when synchronization is enabled.
- Reuse sealed leaves and topic summaries. An unchanged session must not make
  another model call merely because a timer fired.
- Preserve the older topics in the hierarchy and briefly acknowledge material
  earlier work in the overview.

## Non-goals and anti-goals

- No cmux Vault changes, new provider, extra user setting, or model-per-level
  routing.
- No full transcript reread for every open session every five minutes if its
  source file is unchanged.
- No global reanalysis or retitling of historical sessions just to change the
  live-session policy.
- No title derived from process status alone: a running but unchanged agent
  should retain its last successful metadata.
- No overwrite of a manually changed cmux title or description.

## Current constraints

The app already performs a full source discovery scan every 15 minutes in its
local startup script, queues pending analysis through one bounded serialized
worker, and reconciles cmux metadata every minute. cmux matching is by agent
and native session ID. The reconciler pushes stored title and overview only
when analysis is current and retains its existing manual-override behavior.
The scanner's per-session `Rescan` currently rereads the entire file, whereas
full scanning has a size/mtime fast path. Incremental analysis caches summary
nodes by input hash and prompt version. A one-topic session currently adopts
its topic title and summary directly; a multi-topic session uses a root model
request. Thus prompt changes alone would leave old cached roots in place.

## Approaches

1. **Minimal cadence and prompt change:** shorten the full scan interval and
   change the root prompt. This is less code, but repeatedly traverses every
   source and leaves live sessions behind the historical queue. Cached roots
   also need explicit invalidation.
2. **Live-session priority (selected):** keep the existing full discovery
   scan, add a targeted five-minute refresh of matched open sessions, and place
   changed sessions ahead of backlog work. Refresh the root metadata from
   compact summaries, not the full transcript. This adds a narrow scheduling
   path while keeping model cost bounded.

## Behavior and data flow

1. The existing cmux snapshot identifies open, matched agent sessions. Every
   five minutes, the live refresh loop takes a snapshot of those IDs. Only
   existing imported sessions are eligible; full discovery continues to find
   new source sessions. A source file size/mtime check skips the adapter read
   when unchanged. A metadata-only file change that produces the same
   normalized user/assistant conversation is not treated as new analysis work.
2. For a material normalized conversation change, the existing import path
   invalidates only the affected summary suffix. Queue that session on the
   same analysis worker with live priority. Dedupe it against an already
   queued or running job; do not create concurrent analysis of one session.
   Historical pending jobs continue to run, with no starvation from an
   unchanged open session. Full scans and targeted refreshes cannot import the
   same source simultaneously.
3. The analyzer reuses unchanged leaves/topics and creates the session title
   and overview from the current compact topic hierarchy. Prompt input puts
   the newest one to three topics first and gives older topics a bounded
   context budget. The title names the latest substantive work, adding earlier
   context only when necessary to disambiguate. The overview opens with the
   latest goal/outcome, then briefly notes earlier major work. It does not
   imply that an old topic is the current one. Topic chapters remain in
   chronological order in the UI.
4. A single-topic session still needs recency within that topic. Its root
   metadata uses a bounded tail of the topic's existing leaf summaries along
   with the topic summary; it must not simply copy an old topic title when
   newer work in that topic changes the focus. No raw transcript is added to
   this root request.
5. The new root policy gets a root-specific cache identity. Do not bump the
   global prompt version and thereby invalidate reusable leaf/topic nodes.
   Once an open session has been refocused, the same input and policy reuse
   the cached root. On first deployment, currently open sessions with existing
   analysis receive a one-time root refresh from stored summaries, even when
   their source has not grown. Historical closed sessions are left alone.
6. The new title and overview replace Agent History's stored fields and FTS
   document in the existing atomic analysis write. The one-minute cmux
   reconciler then sends those same values according to the user's existing
   sync setting. It still only updates a workspace description when the
   workspace has one mapped agent session, and it still respects manual cmux
   overrides. If analysis fails, the previous visible title/overview remain;
   changed live sessions are eligible for bounded retry, not an every-minute
   model-call loop.

## Verification

- Fake clock/source/cmux tests show that only matched open sessions are
  targeted; unchanged files and tool-only writes cause no model call.
- Queue tests show changed live work runs before queued historical work, while
  deduplication and one-worker serialization hold.
- Analyzer tests cover one topic with a shifting focus, several topics,
  unchanged-root cache reuse, first-deployment root refocus, and a failed root
  generation preserving the previous title and overview.
- cmux tests verify successful auto-sync of both fields, the one-session
  workspace-description rule, disabled sync, and manual override protection.
- Integration tests check that Agent History API/search show the replacement
  metadata and that no raw transcript is sent for root-only refresh.

No test invokes a real model. Use the repository's Go test, race, vet, and
build checks before deploying to the local service.

## Builder choices

The builder should use the existing store, scanner, worker, and reconciler
interfaces wherever possible. The precise queue data structure and root
cache-key representation are implementation choices, provided they preserve
the guarantees above and do not require a broad schema rewrite.
