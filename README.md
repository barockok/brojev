# brojev

A fast browser agent for Claude Code. One Go binary, its own Chrome DevTools Protocol engine, no Playwright, no browser extension.

- **`run`**: hand over a whole browser sub-task. [TypeSafe's Jev](https://docs.typesafe.ai) picks each action (operation and target in one request, about 200 to 350 ms), brojev executes it with real input events, and the task usually finishes in 1 to 10 seconds for a fraction of a cent.
- **Primitives**: snapshot, click, type, select, key, hover, scroll, drag, upload, dialog, tabs, history, wait, screenshot, eval, emulate, state, downloads. Use them when `run` hands back, or for exact control. They cover what Playwright covers: iframes (including cross-site, out-of-process ones), open shadow DOM, JS dialogs, file choosers, downloads, popups, HTML5 and pointer drag, nested scrolling, macOS editing shortcuts, device emulation, cookies and storage.

## Install

Requirements: Google Chrome (or Chromium) on macOS or Linux, and your own TypeSafe API key for `run` (create one at [console.typesafe.ai/keys](https://console.typesafe.ai/keys); usage is billed to your TypeSafe account under [their terms](https://docs.typesafe.ai/legal.md)). The primitives work without a key.

```sh
claude plugin marketplace add barockok/brojev
claude plugin install brojev@brojev
```

At session start the plugin's hook downloads the release binary for your platform (macOS or Linux, arm64 or amd64) into the plugin data directory (`~/.claude/plugins/data/<plugin id>/bin/<version>/`), verifies its checksum, removes older versions, and links it to `~/.brojev/bin/brojev`. Without a matching release it builds from source if Go 1.25+ is installed, and swaps in the release binary once one is available. Uninstalling the plugin removes the data directory.

### API keys

Claude Code asks for the plugin's settings on install: `typesafe_api_key` and the optional `openrouter_api_key` (a helper model that writes form text you did not pass in `values`). Both are marked sensitive, so Claude Code keeps them in your OS keychain and hands them to the brojev MCP server. To set them from the shell instead:

```sh
claude plugin install brojev@brojev --config typesafe_api_key=...
```

Outside Claude Code (the CLI, or if you prefer a file), keys can also come from the environment or `~/.brojev/env`; plugin settings win when both are set:

```sh
mkdir -p ~/.brojev && printf 'TYPESAFE_API_KEY=...\n' > ~/.brojev/env && chmod 600 ~/.brojev/env
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
| TypeSafe (`api.typesafe.ai`) | the goal, the `values` you pass, the page URL, title and snapshot (element roles, names, field values, visible text; password values are masked) | each `run` step |
| OpenRouter (optional) | the goal, the target field and the snapshot | only when `run` must write field text you did not provide, or with `answer` |
| Claude | tool results (snapshots, screenshots, eval output) | every MCP call, as part of your Claude Code conversation |

The primitives (`snapshot`, `click`, ...) call no third-party service. With a personal Chrome profile (attach mode) the snapshot can contain whatever that page shows, including logged-in content: use a dedicated profile for anything you would not paste into a chat.

Local state lives in `~/.brojev` (profiles, downloads, saved cookie/storage state, keys in `env`, daemon log). The daemon listens only on a unix socket readable by your user; launched Chrome exposes its debug port on 127.0.0.1, so other local programs running as you could connect to it while it is open.

## How it works

```
Claude Code --stdio--> brojev mcp --unix socket--> brojevd --one CDP WebSocket--> Chrome (~/.brojev/profile)
                                                      |
                                                      +-- run: snapshot -> 1 Jev request -> validate -> execute -> repeat
```

- **brojevd** is a background daemon that owns the single browser connection, so tabs, refs and logins persist across tool calls and Claude sessions. It is started automatically.
- **Dedicated profile**: Chrome runs on `~/.brojev/profile`, separate from your normal browsing. Log in to sites there once.
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

Profiles (each with its own logins; `*` marks the active one):

```sh
brojev profile                                   # list
brojev profile add work                          # new dedicated profile in ~/.brojev/profiles/work
brojev profile chrome-profiles                   # people inside your real Chrome
brojev profile add me --dir "~/Library/Application Support/Google/Chrome" --chrome-profile "Profile 3"   # attach mode is forced for the real dir
brojev profile use work [--quit]                 # switch; --quit closes the previous profile's Chrome
```

Environment: `BROJEV_HOME` (default `~/.brojev`), `BROJEV_PROFILE` (ad hoc Chrome data dir, overrides the active profile; `BROJEV_CHROME_PROFILE` picks a person in it), `BROJEV_HEADLESS=1`, `BROJEV_ATTACH=1` (connect through `chrome://inspect/#remote-debugging` instead of launching with a debug port), `BROJEV_CHROME` (binary path), `BROJEV_CHROME_ARGS` (extra Chrome flags, space separated), `TYPESAFE_MODEL`, `TEXT_MODEL`, `TEXT_MODEL_BASE_URL`.

## Tests

```sh
go test ./...                                     # unit tests
BROJEV_E2E=1 go test ./internal/page/             # 15 browser tests on local fixtures, headless
BROJEV_E2E=1 go test ./internal/agent/            # Jev loop against a fake Jev server
BROJEV_E2E=1 go test ./internal/mcpserver/        # MCP client -> server -> daemon -> Chrome
brojev bench eval/tasks.json                      # live sites, real Jev (costs fractions of a cent)
python3 eval/claude_bench.py --config brojev=a.json --config playwright=b.json   # inside Claude Code
```

## Known limits

- **`navigator.webdriver` is true** when Chrome is launched with a debug port (Chrome 154 behavior). Some sites may treat the browser as automated. `BROJEV_ATTACH=1` avoids the flag at the cost of one "Allow" click per daemon start.
- Supported on macOS and Linux (arm64, amd64). Windows is not supported yet.
- Closed shadow roots, canvas apps and OS-level UI need the coordinate and screenshot tools.
- Jev is weak at numbers and comparisons ("cheapest", counting, date arithmetic): Claude should do those from snapshots and pass exact values.
- Captchas are handed to the human. brojev does not try to bypass bot protection.

## Credits

Design and several agent rules are adapted from [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast) (MIT, Copyright (c) 2026 Browser Use); brojev is an independent implementation and does not wrap it. The keyboard layout and macOS editing-command tables are ported from [Playwright](https://github.com/microsoft/playwright) (Apache-2.0).
