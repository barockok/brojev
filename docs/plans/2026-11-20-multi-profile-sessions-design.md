# Multi-profile sessions: one daemon, many profiles

Date: 2026-11-20
Status: design validated, not implemented

## Goal

One brojevd serves several Chrome profiles at once. A session is a named
group of windows, and each window is bound to one profile. The agent process
is no longer the unit of identity: one process can run many sessions, on
different profiles, and any client that uses the same session name and
profile gets the same window.

## Today

- The daemon holds one active profile (`s.prof`) and one browser (`s.browser`).
- `profile use` swaps that browser for every session and drops all tabs
  (guarded by `otherLiveBases` and `force`).
- A session id is issued by the brojev-mcp process, not by the agent:
  `$BROJEV_SESSION`, then `$CLAUDE_CODE_SESSION_ID`, then `mcp-<hex>`. The
  per-call `session` param is a subagent suffix (`<base>#sub`).
- Cleanup hangs off a per-process lease (`hello`) with a grace period.

## Model

- **Window key = `(session, profile)`.** `work` on profile A and `work` on
  profile B are two windows in two browsers. The same pair always returns the
  same window.
- A session is a global name that groups windows. It carries no profile and
  no lock.
- Tab ownership (`owner`, `canTouch`, `shared`), the current tab and the
  action queue are per window. Windows run in parallel.
- A call that names no session gets an automatic per-process session (today's
  id), so unrelated agents do not collide by accident. Only an explicitly
  chosen name is shared.

## Browser pool

- `map[userDataDir] -> *browser`, replacing `s.browser` and `s.prof`.
  Attach mode and external CDP are their own keys.
- Chrome cannot open one user-data-dir twice, so two profile names that point
  at the same `Dir` share one browser.
- A browser launches lazily on a window's first action and is refcounted by
  windows. When the last window closes, the browser lingers briefly and then
  quits. The daemon never quits an attach-mode or external Chrome; there it
  closes only its own tabs.
- Caps: the existing `maxSessions`, plus a cap on concurrent browsers.

## Profile resolution

`resolveWindow(session, profile?)`, in order:

1. **Pin.** If the MCP config pins a profile (env var sent in `hello`), the
   profile is that one, and a different `profile` param is an error. The
   daemon enforces this, not the MCP layer.
2. **Explicit `profile` param.** Use that window, creating it if needed. It
   becomes the session's current window.
3. **Sticky.** Profile omitted: the session's most recently used window.
4. **Default.** A new session with no profile: the config's default profile.

Every response echoes the resolved `{session, profile}` so an agent can see
which window it landed on.

Attach-mode profiles keep the existing consent rule and need an explicit
grant before an open (unpinned) agent can use them.

## Lifecycle

- **Named sessions:** a window closes after an idle TTL (default 30 minutes,
  configurable). A later call recreates it. They do not die when the creating
  process disconnects.
- **Auto sessions:** tied to the process lease, cleaned up after the grace
  period, as today.
- **Explicit close:** `session close` closes one window or a whole session.
- **Daemon restart or Chrome crash:** windows are recreated lazily on the next
  call. Tab state is lost, as today.

## Protocol and tools

- Requests carry `{session, profile?}`. The MCP layer fills in the auto id
  when the agent names none. The `#suffix` composition goes away.
- New `session` tool: `list` (windows, profile, tabs, idle time), `close`,
  `ttl`.
- `profile` tool: `list/add/remove/chrome-profiles` stay. `use` only sets the
  default for new sessions; it no longer swaps a global browser.
- `profile list` reports live sessions and browsers per profile.

## Errors

- Unknown profile: list the registered names.
- Pinned process asks for another profile: "this MCP is pinned to X".
- Session or browser cap reached: say which cap and which windows could close.
- Attach profile without a grant: refuse and point at the consent step.

## Testing

- Unit: `resolveWindow` (pin, explicit, sticky, default, same-`Dir` sharing),
  idle TTL with a fake clock, browser refcount and linger.
- Daemon e2e: two sessions on two profiles run in parallel with no tab
  leakage; the same name from two clients reaches the same window.
- Regression: the CLI default session and the single-profile flows behave as
  before.

## Open points for the implementation plan

- Default TTL and linger values.
- Whether `profile use` is kept as a default-setter or removed.
- Migration of `BROJEV_PROFILE` (today it overrides the active profile) into
  the pin semantics.
