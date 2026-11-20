# Cloudflare Turnstile detector and resolver (design)

Date: 2026-11-20
Status: approved for implementation

## Goal

brojev detects a Cloudflare Turnstile challenge on any page it drives and resolves it inside its own harness, so a calling agent (Claude or the Jev loop) never needs a Turnstile-specific action. Detection and resolution are transparent: tool results and run outcomes carry status only when a widget appears or resolution fails.

Decisions from the design discussion:

- Resolution scope: in-page real input first, CapSolver fallback.
- Hook timing: every page call (navigate, history, reload, actions, snapshots, each run step).
- Solver: CapSolver `AntiTurnstileTaskProxyLess`.
- Budget: 60s per challenge by default, `TURNSTILE_TIMEOUT_MS` overrides.
- Failure: the original action result returns unchanged plus a `turnstile` status; no hard error. `run` reports `blocked` with the same reason only when the page is unusable.
- No hourly solver quota. Per-widget bookkeeping only.

Non-goals:

- Full-page Cloudflare interstitials (detected and classified, not solved).
- Other captcha types; those still go to the user.
- Fingerprint spoofing or stealth patching beyond what brojev already does.
- A new Jev operation or a user-facing solve workflow. The harness is automatic.

## Architecture

New code:

- `internal/turnstile/detect.go`: status model, widget state, detection script source.
- `internal/turnstile/solver.go`: `Solver` interface.
- `internal/turnstile/capsolver.go`: CapSolver client (`createTask`, `getTaskResult`).
- `internal/turnstile/capsolver_test.go`: fake HTTP server tests.
- `internal/page/turnstile.go`: page glue. `DetectTurnstile`, `EnsureTurnstile`, in-page click, token polling, token injection.
- `internal/page/turnstile_e2e_test.go`: fixture tests behind `BROJEV_E2E=1`.

Dependency direction: `internal/page` imports `internal/turnstile`. The turnstile package does not import `page` or `cdp`; the solver client is a pure HTTPS client with no browser knowledge.

Per-`Page` state: a resolver mutex, attempt counts keyed by URL plus sitekey, and the time of the last cycle.

### Hooks

- `internal/daemon/tools.go`, `pageTool`: after a successful `navigate`, `back`, `forward`, `reload` or action, and before building a snapshot-bearing reply.
- Daemon `snapshot`: before the snapshot is taken.
- `internal/agent/agent.go`: after the optional initial `Navigate`, and at the top of each loop iteration before `Snapshot`.
- `batch` is covered automatically because every step runs through `pageTool`.
- Tools that do not drive a page (`tabs`, `downloads`, `profile`, `state`, `emulate`, `dialog`) are not hooked. `eval` runs the check after evaluation because it can change the DOM.

## Detection

One isolated-world eval in the main frame returns the widget model:

```json
{
  "present": true,
  "sitekey": "0x4AAAAAAA...",
  "action": "login",
  "cdata": "",
  "mode": "managed",
  "token": "",
  "widget": { "x": 120, "y": 540, "w": 300, "h": 65 },
  "error": ""
}
```

Sources, in order of preference:

- Widget root: `div.cf-turnstile`, `[data-sitekey]`.
- `sitekey`, `action`, `cdata`: `data-sitekey`, `data-action`, `data-cdata` attributes on the widget root.
- Widget frame: `iframe[src^="https://challenges.cloudflare.com/"]` inside or adjacent to the root.
- Response value: `input[name="cf-turnstile-response"]`, then `textarea[name="cf-turnstile-response"]`, then any `[name="cf-turnstile-response"]`.
- Mode: `managed` when an interactive widget frame is visible, `invisible` when the root exists with no visible frame, `unknown` otherwise.
- Error: visible widget error text, for example "Verification failed", or a Turnstile error code when one is detectable.

A full-page Cloudflare interstitial (title "Just a moment...", `#challenge-stage`, or a challenge URL pattern) is classified as `interstitial` and is never sent to the Turnstile solver.

External state values:

- `absent`: no widget on the page.
- `solved`: a non-empty response token exists.
- `unresolved`: a widget exists but no token after the harness ran.
- `interstitial`: a full-page challenge is present.

Internal widget state (`waiting`, `verifying`, `error`) is kept for diagnostics and the `reason` text.

Cost when absent: one eval in the main frame, about 2 to 5 ms. Frame enumeration and solver work happen only when a widget is present.

Status is invalidated on navigation, when the response input disappears, or when its value becomes empty after being non-empty (the page consumed the token). The URL plus sitekey key prevents repeated solving of the same widget instance.

## Resolution flow

When state is `waiting` or `error`:

### 1. In-page attempt (up to 8 seconds)

- Bring the tab to front if it is not already; background tabs do not hit-test.
- Find the Turnstile frame among the page frames (URL starts with `https://challenges.cloudflare.com/`).
- Inside that frame, look for `input[type="checkbox"]`, `[role="checkbox"]`, or an element whose accessible name contains "Verify you are human".
- If found, compute its top-level viewport point through the existing frame offsets and click with real CDP mouse events. If the inner DOM is closed or no checkbox is found, click the widget's left-center with a few small offset fallbacks.
- Poll every 250 ms for a non-empty response token. A managed or invisible widget may pass without any click, so the poll runs even when no checkbox was found.
- On `error`, reset the widget with `window.turnstile.reset()` through main-world `Page.Eval` when the API is present, otherwise click the widget's refresh control, then retry once.

### 2. CapSolver fallback (rest of the budget)

Runs only when `CAPSOLVER_API_KEY` is set, the state is not `solved`, and a sitekey is known.

- `POST https://api.capsolver.com/createTask` with:

```json
{
  "clientKey": "<key>",
  "task": {
    "type": "AntiTurnstileTaskProxyLess",
    "websiteURL": "<page URL>",
    "websiteKey": "<sitekey>",
    "action": "<optional>",
    "cdata": "<optional>"
  }
}
```

- Poll `POST https://api.capsolver.com/getTaskResult` every 2 seconds until ready or the budget expires.
- On ready, read `solution.token`.
- Inject the token into `input[name="cf-turnstile-response"]` through main-world eval. Use the native value setter, then dispatch `input` and `change` events with `bubbles: true`. If no input exists, create a hidden input inside the widget container and set it the same way.
- This covers standard form posts. Callback-only widgets that never read the input may remain unresolved; the status reports that instead of claiming success.

### 3. Verification

Re-run detection. A non-empty response token is the only success criterion. The widget's own state or callback is never trusted instead.

### Bookkeeping

- One in-page cycle plus one solver task per widget instance.
- At most two cycles per URL plus sitekey per page load.
- One second cooldown between cycles.
- This prevents loops on always-failing keys, such as Cloudflare's `2x...` test key, and bounds spend to one solver task per cycle.
- A per-page mutex serializes resolution for a tab. Different tabs solve independently.

### Budget

- `TURNSTILE_TIMEOUT_MS` default 60000, measured from the start of the first resolution attempt for a challenge.
- In-page phase capped at 8 seconds.
- Solver polling gets the remainder.
- Context cancellation aborts polling and returns the current status.

### Failure

- The original action result returns unchanged.
- When a widget was detected, the reply gains a `turnstile` object: `detected`, `sitekey`, `action`, `mode`, `state`, `attempts`, `solverUsed`, `reason`.
- The rendered reply gets one short note line, for example `TURNSTILE: unsolved (in-page failed, solver disabled)`.
- No hard error, since an unrelated click should not fail because a background widget did not solve.
- `run` returns `blocked` with `turnstile unsolved: <reason>` only when the page has no usable interactive content: an interstitial, or a page whose only actionable content is the widget.

## Configuration

- `CAPSOLVER_API_KEY`: environment, `~/.brojev/env`, or a new sensitive `capsolver_api_key` plugin setting.
- Plumbed like existing keys: `.claude-plugin/plugin.json` `userConfig`, `.mcp.json` environment as `BROJEV_SETTING_CAPSOLVER_API_KEY`, and `keyNames` in `internal/daemon/keys.go` so every session forwards it to a daemon that predates the key.
- `TURNSTILE_TIMEOUT_MS`, default 60000.
- The solver is enabled only when a key is configured. Without one, brojev does the in-page attempt and reports status; it never silently skips detection.
- `brojev doctor` adds one line: `capsolver: configured` or `capsolver: not configured (in-page only)`. The key itself is never printed.
- No new MCP tool. Status rides on existing replies and the run outcome.
- When a widget is detected, the rendered snapshot includes one line: `TURNSTILE: solved` or `TURNSTILE: unresolved (<reason>)`.
- Detection runs in the isolated world because it only reads the DOM. Reset, injection and event dispatch use main-world `Page.Eval`, where `window.turnstile` and the page's own event handlers live.

## Cost and privacy

- CapSolver lists Cloudflare Turnstile at $1.20 per 1,000 tasks, so about $0.0012 per solve. Checked 2026-11-20 at `https://docs.capsolver.com/en/pricing/`.
- The in-page attempt costs nothing and should resolve most managed widgets. The solver runs only on fallback, at most once per cycle.
- Sent to CapSolver: page URL, sitekey, action, cdata. Nothing else.
- Never sent: cookies, storage, form values, page text, screenshots, the CapSolver key in logs.
- The key lives where the other brojev keys live: plugin settings in the OS keychain, or `~/.brojev/env` with mode 0600.
- The README privacy table gains a CapSolver row.

## Testing

Unit tests:

- `internal/turnstile`: CapSolver client against `httptest` for success, API error, polling timeout, and context cancellation. No network and no paid calls.
- Detection parsing and status transitions where they can be exercised without a browser.

E2E tests, gated by `BROJEV_E2E=1`:

- Local fixture pages using Cloudflare's published test sitekeys:
  - `1x00000000000000000000AA`: always passes.
  - `2x00000000000000000000AB`: always blocks.
  - `3x00000000000000000000FF`: forces an interactive challenge.
- Cases: detection reports present, sitekey and mode; `1x` reaches `solved` without a click; `3x` exercises the click path; `2x` ends `unresolved` with a reason and does not loop; token injection through a fake solver sets the response input and moves the state to `solved`.
- The fixture needs `challenges.cloudflare.com`; skip with a clear message if it is unreachable.
- Existing page, agent, MCP and browser suites must stay green.

Manual check: run a real Turnstile page in the brojev Chrome window with and without `CAPSOLVER_API_KEY`.

## Documentation

- README: behavior, config, cost, failure semantics, limits, and what leaves the machine.
- Privacy table: CapSolver row.
- `skills/brojev/SKILL.md`: replace "never try to bypass captchas" with auto-handling, the status shape, and a rule not to retry in a loop. Other captcha types still go to the user.
- `docs/ARCHITECTURE.md`: replace "no solver services" with the new harness and its hooks.
- Release process stays unchanged: version bump and tag happen at release time.

## Risks and limits

- Turnstile can still reject automated browsing even with trusted CDP input. The in-page attempt is best effort, and the solver covers most of what remains.
- Explicit-render widgets without a `data-sitekey` attribute cannot be sent to CapSolver. The status reports `sitekey missing`.
- Token injection does not fire widget callbacks. Standard form posts work; callback-only integrations may stay unresolved.
- Widget DOM, iframe URLs and the CapSolver API can change. Keep selectors and endpoint constants centralized in `internal/turnstile`.
- Full-page Cloudflare interstitials are out of scope and are reported as `interstitial`.
- Solving a challenge on a site may conflict with that site's terms. The solver is opt-in through a user-provided key, and the README states that it is sent to CapSolver only when configured.

## Implementation outline

1. `internal/turnstile`: status model, solver interface, CapSolver client, unit tests.
2. `internal/page/turnstile.go`: detection, in-page click, polling, injection.
3. Hooks in `internal/daemon/tools.go`, the daemon snapshot path, and `internal/agent/agent.go`.
4. Reply and outcome plumbing: `turnstile` in `ActionReply`, the run `Outcome`, and the rendered snapshot line.
5. Config plumbing: plugin setting, `.mcp.json`, `keyNames`, `doctor`.
6. E2E fixtures and tests with Cloudflare test sitekeys.
7. Docs and skill updates.
8. Full regression: unit tests, E2E suites, and a manual real-page check.
