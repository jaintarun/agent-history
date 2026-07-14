# Web Auto-Refresh Design

## Goal

Keep the session results and selected-session detail current without changing
the user's search, filters, URL state, or selection.

## Behavior

- Add a header button labeled `Refresh in 60s`.
- Update its countdown once per second.
- Refresh immediately when the button is clicked.
- Refresh automatically when the countdown reaches zero.
- During a refresh, label the button `Refreshing...` and prevent another manual
  refresh.
- Use the existing session loader, which reapplies the current form filters and
  reloads the selected session detail when a selection exists.
- Restart the 60-second countdown after each manual or automatic refresh.
- Leave the existing faster polling for a selected queued or running analysis
  unchanged.

## Scope

This is an embedded-frontend change only. It adds no HTTP endpoint, server
configuration, persistence, push channel, or refresh-frequency setting.

## Verification

- An embedded-asset contract test checks that the refresh control and refresh
  behavior are shipped in the binary.
- Existing Go tests, race tests, vet, and build checks remain green.
- Browser verification covers the countdown, manual refresh, preserved filters
  and selection, automatic refresh, and desktop and narrow layouts.
