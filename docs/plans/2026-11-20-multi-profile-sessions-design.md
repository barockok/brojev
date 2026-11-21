# Multi-profile sessions: one daemon, many profiles

Date: 2026-11-20 (revised 2026-11-21)
Status: implemented on `feat/multi-profile-sessions`

## Goal

One brojevd serves several Chrome profiles at once. An agent can work on one
profile or on several side by side, and a user can pin an agent to a single
profile from the MCP config.

## Model

- **One daemon, many profiles.** Each profile in use has its own Chrome.
- **A session is one window on one profile.** The profile is set when the
  session starts and never changes: a window cannot be in two profiles.
- **The agent chooses.** Every tool takes `session` (a name the agent picks)
  and `profile` (optional).
  - Same session name again: back to the same window.
  - New name, same profile: a second window in that Chrome.
  - New name, another profile: a window in another Chrome.
  - An existing session with another profile: an error that names both.
- **No session name** means "my default window on that profile". A caller has
  one such window per profile, so `profile=work` without a name still works.
- **No profile** means the default profile (`profile use` sets it). Changing
  the default only affects sessions started later; running sessions keep
  their profile and their tabs.
- **Pin.** With `BROJEV_PIN_PROFILE` in the MCP server's env (or the plugin
  setting "Pin to one Chrome profile"), every session of that server runs on
  the pinned profile. Another `profile` is refused, and the `profile` tool can
  only list. Without a pin the agent chooses freely.

Session names stay private to each agent process, as before, so two Claude
Code instances that both pick `research-1` do not share a window.

## Daemon

- `Server.links`: one `link` per Chrome data dir, holding the CDP connection,
  the attached pages and the tab owners. This replaces the single
  `browser`/`prof`/`pages`/`owner` of the server.
- Each link has its own connect lock, so an attach-mode profile waiting for
  the user's Allow click does not block the other profiles.
- A session holds its profile and its link. Tab ownership, the current tab
  and the action queue are per session, as before.
- Chrome runs one instance per data dir. Profiles on the same dir and the same
  person share a link. A different person in a dir that sessions are using is
  refused; an idle dir is switched, as `profile use` did before.
- A dropped connection (Chrome quit) forgets only that link's tabs.
- `BROJEV_CDP_URL` and `BROJEV_PROFILE` put the daemon on one browser; a
  profile chosen per session is refused there.
- `profile use` no longer disconnects anything. `--quit` closes the previous
  default's Chrome and is refused while other sessions work there (unless
  `force`).
- `status` takes `profile` and lists the live browsers with their sessions.
- The default state file carries the profile name, so logins saved from one
  profile are not loaded into another by default.

## MCP layer

- `Sess` (embedded in every tool input) has `session` and `profile`.
- `applyPin` runs on every daemon call: it sets the pinned profile, refuses
  another one, and makes the `profile` tool read-only. The pin guards the
  agent, not the socket: any local process of the same user can still talk to
  the daemon directly, as it always could.

## Cleanup

Unchanged: when an agent process goes away, its windows close after the grace
period (not in attach mode or on an external Chrome). Chrome itself keeps
running, as before.

## Not done

- No cap on the number of Chromes. The session cap (8) bounds it in practice.
- The daemon does not quit an idle profile's Chrome on its own.
- Downloads of all profiles still go to one `~/.brojev/downloads` directory.
- Running two people of the same Chrome data dir at once.

## Tests

- Unit: `TestSessionIsLockedToItsProfile`, `TestUnnamedSessionPerProfile`,
  `TestSharedDataDir`, `TestExternalChromeHasNoProfiles`, `TestResolve`,
  `TestApplyPin`, `TestPinFromEnv`.
- End to end (`BROJEV_E2E=1`, real Chrome): `TestProfilesSideBySide`,
  `TestPinnedServer`, `TestSessionLeaseCleansUp`.
