# brojev

A fast browser agent for Claude Code. One Go binary, its own Chrome DevTools Protocol engine, no Playwright, no browser extension.

- **`run`**: hand over a whole browser sub-task. [TypeSafe's Jev](https://docs.typesafe.ai) (through OpenRouter) picks each action (operation and target in one request, about 300 to 550 ms through OpenRouter), brojev executes it with real input events, and the task usually finishes in 1 to 10 seconds for a fraction of a cent.
- **`batch`**: several primitive steps you already know (fill a form, set filters, submit) in one call: no snapshot between steps, stops at the first failure, one snapshot at the end. Fewer agent turns than one primitive per call.
- **Primitives**: snapshot, click, type, select, key, hover, scroll, drag, upload, dialog, tabs, history, wait, screenshot, eval, emulate, state, downloads. Use them when `run` hands back, or for exact control. They cover what Playwright covers: iframes (including cross-site, out-of-process ones), open shadow DOM, JS dialogs, file choosers, downloads, popups, HTML5 and pointer drag, nested scrolling, macOS editing shortcuts, device emulation, cookies and storage.

## Install

Requirements: Google Chrome (or Chromium) on macOS or Linux, and your own [OpenRouter API key](https://openrouter.ai/settings/keys) for `run`, billed to your OpenRouter account. The primitives work without a key.

```sh
claude plugin marketplace add barockok/brojev
claude plugin install brojev@brojev
```

Then start Claude Code and run `/brojev:setup`: it checks Chrome and the daemon, walks you through the API key and the Chrome profile choice, and runs a smoke test.

At session start the plugin's hook downloads the release binary for your platform (macOS or Linux, arm64 or amd64) into the plugin data directory (`~/.claude/plugins/data/<plugin id>/bin/<version>/`), verifies its checksum, removes older versions, and links it to `~/.brojev/bin/brojev`. Without a matching release it builds from source if Go 1.25+ is installed, and swaps in the release binary once one is available. Uninstalling the plugin removes the data directory.

### API keys

`run` needs one key: `openrouter_api_key`. It pays for Jev (OpenRouter serves TypeSafe's System One API at TypeSafe's own token price, $0.042 per million input tokens, output free) and for the helper model that writes form text you did not pass in `values`. Claude Code asks for it on install, marks it sensitive, keeps it in your OS keychain and hands it to the brojev MCP server. To set it from the shell instead:

```sh
claude plugin install brojev@brojev --config openrouter_api_key=...
```

Optional: a `typesafe_api_key` ([console.typesafe.ai/keys](https://console.typesafe.ai/keys)) lets brojev call Jev at TypeSafe directly, which saves about 200 ms per step; the helper still uses OpenRouter. The `jev_provider` setting (`JEV_PROVIDER` outside Claude Code) picks the route:

| `jev_provider` | Jev is called at | Needs |
|---|---|---|
| `auto` (default) | TypeSafe when a TypeSafe key is set, otherwise OpenRouter | either key |
| `openrouter` | OpenRouter, even if a TypeSafe key is set | `openrouter_api_key` |
| `typesafe` | TypeSafe | `typesafe_api_key` |

Change it in the plugin settings (`/plugin`), or set it at install with `--config jev_provider=typesafe`; the next `run` uses it, and `brojev doctor` shows the active route.

Outside Claude Code (the CLI, or if you prefer a file), keys can also come from the environment or `~/.brojev/env`; plugin settings win when both are set:

```sh
mkdir -p ~/.brojev && printf 'OPENROUTER_API_KEY=...\n' > ~/.brojev/env && chmod 600 ~/.brojev/env
export PATH="$HOME/.brojev/bin:$PATH"   # optional: the brojev CLI
brojev doctor                           # checks Chrome, daemon, connection, keys
```

### Updating

Claude Code turns auto-update off by default for third-party marketplaces, so new brojev releases arrive only when you ask:

```sh
claude plugin marketplace update brojev
claude plugin update brojev@brojev
```

Or turn on auto-update for the `brojev` marketplace in `/plugin` (Marketplaces). Each release is a new plugin version pinned to its git tag; the next session start installs the matching binary, removes the old one, and replaces a running brojev daemon from the previous version (Chrome keeps running; an attached personal Chrome asks for "Allow" once more).

In Claude Code the tools appear as `mcp__plugin_brojev_brojev__run`, `..._snapshot`, and so on, and the `brojev` skill explains when to use which.

On Linux, brojev adds `--no-sandbox` when it runs as root (containers), since Chrome refuses to start sandboxed as root. On a machine without a display, set `BROJEV_HEADLESS=1` (in the environment or `~/.brojev/env`) so the daemon starts Chrome headless.

### From source

```sh
git clone https://github.com/barockok/brojev && cd brojev
claude --plugin-dir .        # rebuilds build/brojev whenever the source changes
go test ./...                # unit tests
BROJEV_E2E=1 go test ./internal/page/ ./internal/mcpserver/   # against real headless Chrome
```

Releases:

1. Set the new version in `.claude-plugin/plugin.json` and the matching `ref` (`v<version>`) in `.claude-plugin/marketplace.json`; `scripts/check-version.sh` verifies they agree (CI runs it too).
2. Commit, tag `v<version>`, and push the commit and the tag together.
3. The release workflow checks the tag against both files, runs the tests, and publishes the binaries with `checksums.txt` through GoReleaser. Users get the update via the marketplace (see Updating).

## Data and privacy

brojev runs locally. What leaves your machine:

| Call | Sent | When |
|---|---|---|
| Jev, through OpenRouter (`openrouter.ai`) to TypeSafe, or `api.typesafe.ai` directly with a TypeSafe key | the goal, the `values` you pass, the page URL, title and snapshot (element roles, names, field values, visible text; password values are masked) | each `run` step |
| Helper model on OpenRouter | the goal, the target field and the snapshot | only when `run` must write field text you did not provide, or with `answer` |
| Jev with an image (`TURNSTILE_VISUAL=jev`) | a clipped PNG around the Turnstile widget | only when the in-page click fails and the visual phase is enabled |
| CapSolver (`api.capsolver.com`) | page URL, Turnstile sitekey, `data-action`, `data-cdata` | only when an in-page Turnstile attempt fails and `CAPSOLVER_API_KEY` is set |
| Claude | tool results (snapshots, screenshots, eval output) | every MCP call, as part of your Claude Code conversation |

The primitives (`snapshot`, `click`, ...) call no third-party service unless the Turnstile resolver runs its visual or CapSolver fallback. With a personal Chrome profile (attach mode) the snapshot can contain whatever that page shows, including logged-in content: use a dedicated profile for anything you would not paste into a chat.

Local state lives in `~/.brojev` (profiles, downloads, saved cookie/storage state, keys in `env`, daemon log). The daemon listens only on a unix socket readable by your user; launched Chrome exposes its debug port on 127.0.0.1, so other local programs running as you could connect to it while it is open.

## Cloudflare Turnstile

brojev detects Turnstile widgets and resolves them itself, on every page call: after navigation and each action, before snapshots, and inside every `run` step. The calling agent does not get a captcha step.

1. In-page attempt: brojev clicks the widget's checkbox inside its iframe, or the widget's left-center when it draws in a closed shadow root without a frame, and waits up to 8 seconds for the `cf-turnstile-response` token. Widgets that pass without interaction are picked up by the same poll.
2. Visual click (opt-in): when `TURNSTILE_VISUAL=jev` is set and the in-page attempt fails, brojev captures a clipped PNG around the widget, asks Jev for the checkbox box, `noul` presence and widget state, then clicks the returned box. Cost is about $0.00005 to $0.00025 per attempt and latency is a few hundred milliseconds. This is useful when a theme, language or layout moves the checkbox away from the blind left-center point. It needs a Jev key.
3. CapSolver fallback: if the earlier steps fail and `CAPSOLVER_API_KEY` is set, brojev sends the page URL, sitekey, `data-action` and `data-cdata` to CapSolver (`AntiTurnstileTaskProxyLess`), then injects the returned token into the response input. CapSolver lists Turnstile at $1.20 per 1,000 solves, so about $0.0012 per task. Without a key, brojev stays in-page and visual only.

Full-page challenges ("Just a moment...", "Performing security verification") are handled too. Their checkbox sits in a `challenges.cloudflare.com` frame, usually inside a closed shadow root, and they pass by leaving the challenge page rather than by a token. brojev waits up to 8 seconds for that frame through the CDP frame tree (many challenges pass on their own meanwhile), clicks the checkbox inside it, or the frame's left-center when the checkbox is out of reach, and then, with `TURNSTILE_VISUAL=jev`, runs the visual click on a clip around the frame (the middle of the viewport when no frame renders). Success is the challenge page going away. CapSolver is not used here: a form token does not clear a full-page challenge.

The whole attempt is bounded by `TURNSTILE_TIMEOUT_MS` (default 60000), with at most two attempts per widget (or per challenged host) on a page. When a widget is present, replies and snapshots carry one line:

```
TURNSTILE: solved
TURNSTILE: unresolved (in-page attempt failed; CapSolver not configured)
TURNSTILE: interstitial (full-page Cloudflare challenge; in-page click did not pass; visual: checkbox not located (present=false conf=0.00))
```

Limits: a full-page challenge that never renders its frame, or that rejects the browser or its IP whatever is clicked, stays unresolved and is reported; `run` then returns `blocked` with the reason. Token injection covers standard form posts; callback-only widgets that never read the response input may stay unresolved. The visual phase was validated against Cloudflare's `3x` key and local widgets through TypeSafe; image routing through OpenRouter is untested. Set `CAPSOLVER_API_KEY` in `~/.brojev/env` or the plugin settings, and `TURNSTILE_VISUAL=jev` in `~/.brojev/env` or the environment, to enable the fallbacks for every session; `brojev doctor` shows what is configured.

## How it works

```
Claude Code --stdio--> brojev mcp --unix socket--> brojevd --one CDP WebSocket per profile--> Chrome (~/.brojev/profile, ...)
                                                      |
                                                      +-- run: snapshot -> 1 Jev request -> validate -> execute -> repeat
```

- **brojevd** is a background daemon that owns the browser connections (one per Chrome profile in use), so tabs, refs and logins persist across tool calls and Claude sessions. It is started automatically.
- **Dedicated profile**: Chrome runs on `~/.brojev/profile`, separate from your normal browsing. Log in to sites there once.
- **Sessions and profiles**: a session is one browser window on one profile. An agent names a session to get a window of its own and passes `profile` to open it on another profile, so one agent can work on several profiles side by side, each in its own Chrome. A session never changes profile. To keep an agent on a single profile, set the plugin's "Pin to one Chrome profile" setting (or `BROJEV_PIN_PROFILE` in the MCP server's env): it then cannot pick another profile or change the profile list.
- **Snapshot**: one isolated-world script per frame, all frames in parallel. Elements get stable refs (`e12`, `f2e5` for frame 2). Names follow the accessibility name rules, so a calendar cell reads "Friday, November 20, 2026".
- **Actionability**: before input, the target is scrolled into view, checked for a stable box, and hit-tested at up to five points. Clicks on covered elements are refused instead of landing on an overlay.
- **Jev loop**: per step, one request carries an operation question plus one target question per operation (CLICK, TYPE_TEXT, SELECT, HOVER, DRAG, UPLOAD, PRESS_KEY, SCROLL, BACK, WAIT, dialog accept/dismiss, DONE, BLOCKED). Options are restricted by construction to elements the operation can act on. Answers are validated before anything executes. Values Claude provides become field/value pairs, so typing needs no text model. Low confidence, stalls, or repeated failures hand control back to Claude with the current page.

## CLI

```sh
brojev run --url https://en.wikipedia.org --value "search=Gödel's incompleteness theorems" "Open the article about Gödel's incompleteness theorems"
brojev snapshot | brojev click e12 | brojev type e4 "Zurich" | brojev key Enter | brojev screenshot
brojev bench eval/tasks.json           # Jev loop on the live task suite
brojev stop                            # stop the daemon (Chrome keeps running)
```

Profiles (each with its own logins and its own Chrome; `*` marks the default one, used by sessions that name no profile):

```sh
brojev profile                                   # list
brojev profile add work                          # new dedicated profile in ~/.brojev/profiles/work
brojev profile chrome-profiles                   # people inside your real Chrome
brojev profile add me --dir "~/Library/Application Support/Google/Chrome" --chrome-profile "Profile 3"   # attach mode is forced for the real dir
brojev profile use work [--quit]                 # make it the default; --quit closes the previous default's Chrome
brojev navigate '{"url":"https://example.com","profile":"work"}'   # one call on another profile
```

Environment: `BROJEV_HOME` (default `~/.brojev`), `BROJEV_PROFILE` (ad hoc Chrome data dir for the daemon, which then drives only that one and refuses a profile chosen per session; `BROJEV_CHROME_PROFILE` picks a person in it), `BROJEV_PIN_PROFILE` (in the MCP server's env: the only profile that server's agents may use), `BROJEV_HEADLESS=1`, `BROJEV_ATTACH=1` (connect through `chrome://inspect/#remote-debugging` instead of launching with a debug port), `BROJEV_CHROME` (binary path), `BROJEV_CHROME_ARGS` (extra Chrome flags, space separated), `BROJEV_CDP_URL` (DevTools HTTP base of a Chrome you run, e.g. `http://127.0.0.1:9223`: the daemon attaches to it and never launches, quits or relaunches it; choosing or switching profiles is refused; the daemon keeps one idle `about:blank#brojev-guard` tab open, hidden from `tabs`, so the window never closes), `BROJEV_NO_SPAWN=1` (clients never start or replace the daemon and `brojev stop` is refused; they fail if it is not running or runs another version. The daemon itself still accepts `shutdown` from any socket client), `CAPSOLVER_API_KEY` (optional Turnstile fallback), `TURNSTILE_VISUAL` (`jev` adds the image click before CapSolver), `TURNSTILE_TIMEOUT_MS` (default 60000), `TYPESAFE_MODEL`, `JEV_PROVIDER` (`auto`, `openrouter` or `typesafe`), `JEV_ENDPOINT` (Jev URL override), `TEXT_MODEL`, `TEXT_MODEL_BASE_URL`.

## Shared host daemon

One daemon on a host can serve several clients, for example agents in containers, so they share one browser and its logins:

- **Daemon**: runs on the host as one uid, with `BROJEV_CDP_URL` pointing at a Chrome the operator starts and manages (e.g. `--remote-debugging-port=9223 --remote-debugging-address=127.0.0.1`). brojev never launches or quits that Chrome; its guard tab keeps the window open when every other tab closes, and profile switching is refused.
- **Clients**: run `brojev mcp` as the same uid, with `BROJEV_HOME` pointing at the bind-mounted directory that holds the host's `brojevd.sock`, and `BROJEV_NO_SPAWN=1` so they never start a daemon of their own. Keep that path short (100 bytes or less including `/brojevd.sock`) on both sides, or the socket falls back to a per-home path in `/tmp`.
- **Permissions**: the socket is mode 0600, so the client uid must match the daemon uid.
- **Versions**: the host daemon and the client binary must be the same version. Under `BROJEV_NO_SPAWN` a mismatch is a hard error instead of a daemon replacement, so upgrade both together.
- **Limit**: the daemon still accepts `shutdown` from any socket client (`brojev stop` is refused only client side). Daemon-side policy is future work.

```sh
# host
BROJEV_HOME=/srv/brojev BROJEV_CDP_URL=http://127.0.0.1:9223 brojev daemon
# container, same uid, /srv/brojev bind-mounted
BROJEV_HOME=/srv/brojev BROJEV_NO_SPAWN=1 brojev mcp
```

## Tests

```sh
go test ./...                                     # unit tests
BROJEV_E2E=1 go test ./internal/page/             # 15 browser tests on local fixtures, headless
BROJEV_E2E=1 go test ./internal/agent/            # Jev loop against a fake Jev server
BROJEV_E2E=1 go test ./internal/mcpserver/        # MCP client -> server -> daemon -> Chrome
BROJEV_E2E=1 go test ./internal/daemon/           # BROJEV_CDP_URL against a Chrome the test runs
brojev bench eval/tasks.json                      # live sites, real Jev (costs fractions of a cent)
python3 eval/claude_bench.py --config brojev=a.json --config playwright=b.json   # inside Claude Code
```

## Known limits

- **`navigator.webdriver` is true** when Chrome is launched with a debug port (Chrome 154 behavior). Some sites may treat the browser as automated. `BROJEV_ATTACH=1` avoids the flag at the cost of one "Allow" click per daemon start.
- Supported on macOS and Linux (arm64, amd64). Windows is not supported yet.
- Closed shadow roots, canvas apps and OS-level UI need the coordinate and screenshot tools.
- Jev is weak at numbers and comparisons ("cheapest", counting, date arithmetic): Claude should do those from snapshots and pass exact values.
- **Cloudflare Turnstile** is detected and resolved automatically (in-page click, the opt-in visual click, then CapSolver when a key is set), and so are full-page Cloudflare challenges (in-page and visual click). A challenge that still does not pass, and other captcha types, go to the user; brojev does not patch fingerprints or spoof browser signals.

## Credits

Design and several agent rules are adapted from [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast) (MIT, Copyright (c) 2026 Browser Use); brojev is an independent implementation and does not wrap it. The keyboard layout and macOS editing-command tables are ported from [Playwright](https://github.com/microsoft/playwright) (Apache-2.0).
