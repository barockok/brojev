# brojev

A fast browser agent for Claude Code. One Go binary, its own Chrome DevTools Protocol engine, no Playwright, no browser extension.

- **`run`**: hand over a whole browser sub-task. [TypeSafe's Jev](https://docs.typesafe.ai) picks each action (operation and target in one request, about 200 to 350 ms), brojev executes it with real input events, and the task usually finishes in 1 to 10 seconds for a fraction of a cent.
- **Primitives**: snapshot, click, type, select, key, hover, scroll, drag, upload, dialog, tabs, history, wait, screenshot, eval, emulate, state, downloads. Use them when `run` hands back, or for exact control. They cover what Playwright covers: iframes (including cross-site, out-of-process ones), open shadow DOM, JS dialogs, file choosers, downloads, popups, HTML5 and pointer drag, nested scrolling, macOS editing shortcuts, device emulation, cookies and storage.

## Install

Requirements: Google Chrome, Go 1.25+ (the plugin builds the binary on first use), a TypeSafe API key.

```sh
# keys (either the environment or this file)
mkdir -p ~/.brojev && cat > ~/.brojev/env <<'EOF'
TYPESAFE_API_KEY=...
OPENROUTER_API_KEY=...   # optional: helper that writes field text not covered by `values`
EOF
chmod 600 ~/.brojev/env

# as a Claude Code plugin
claude --plugin-dir /path/to/brojev            # quick, per session
# or install from the bundled local marketplace
claude plugin marketplace add /path/to/brojev
claude plugin install brojev@brojev-local

brojev doctor   # checks Chrome, daemon, connection, keys (bin/brojev after the first build)
```

In Claude Code the tools appear as `mcp__plugin_brojev_brojev__run`, `..._snapshot`, and so on, and the `brojev` skill explains when to use which.

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

Environment: `BROJEV_HOME` (default `~/.brojev`), `BROJEV_PROFILE` (ad hoc Chrome data dir, overrides the active profile; `BROJEV_CHROME_PROFILE` picks a person in it), `BROJEV_HEADLESS=1`, `BROJEV_ATTACH=1` (connect through `chrome://inspect/#remote-debugging` instead of launching with a debug port), `BROJEV_CHROME` (binary path), `TYPESAFE_MODEL`, `TEXT_MODEL`, `TEXT_MODEL_BASE_URL`.

## Tests

```sh
go test ./...                                     # unit tests (CDP client, keyboard)
BROJEV_E2E=1 go test ./internal/page/             # 15 browser tests on local fixtures, headless
BROJEV_E2E=1 go test ./internal/agent/            # Jev loop against a fake Jev server
BROJEV_E2E=1 go test ./internal/mcpserver/        # MCP client -> server -> daemon -> Chrome
brojev bench eval/tasks.json                      # live sites, real Jev (costs fractions of a cent)
python3 eval/claude_bench.py --config brojev=a.json --config playwright=b.json   # inside Claude Code
```

## Known limits

- **`navigator.webdriver` is true** when Chrome is launched with a debug port (Chrome 154 behavior). Some sites may treat the browser as automated. `BROJEV_ATTACH=1` avoids the flag at the cost of one "Allow" click per daemon start.
- Closed shadow roots, canvas apps and OS-level UI need the coordinate and screenshot tools.
- Jev is weak at numbers and comparisons ("cheapest", counting, date arithmetic): Claude should do those from snapshots and pass exact values.
- Captchas are handed to the human. brojev does not try to bypass bot protection.

## Credits

Ideas from [browser-use/jev-ultrafast](https://github.com/browser-use/jev-ultrafast) (MIT). The keyboard layout and macOS editing-command tables are ported from [Playwright](https://github.com/microsoft/playwright) (Apache-2.0).
