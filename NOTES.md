# brojev

Graduated 2026-10-12 from ~/Second-Brain/workspace/brojev to ~/brojev (own git repo). Wiki: ~/Second-Brain/wiki/ent-brojev.md

Started: 2026-10-12

## Goal
Claude Code plugin ("browser jev"): super-fast browser agent. Claude hands off whole sub-goals; a Jev-driven loop executes them. Own implementation (learn from jev-ultrafast, do NOT wrap it). Ideally no Playwright / Chrome-extension dependency, but must be able to do anything Playwright or a human can do.

## Current state
- jev-ultrafast study done: study-jev-ultrafast.md (HEAD 1231850)
- KEY: operation set is client-defined (sent as Choice criteria; API is generic, max 255 options/Choice). We can add PRESS_KEY, HOVER, UPLOAD, DRAG, SWITCH_TAB etc. Params need enum heads, code, or helper LLM (Jev cannot generate).

## M0 status (2026-10-12): DONE
- Go module github.com/barockok/brojev, deps: coder/websocket, chromedp/cdproto v0.157.9 (types via cdp.Session interface, our client implements it)
- internal/cdp: own CDP client (flat sessions, 30s default timeout, unbounded event buffers). 5 unit tests vs fake WS server, -race clean
- internal/chrome: find/launch dedicated profile (--remote-debugging-port=0, no --enable-automation, anti-throttle flags), reattach via DevToolsActivePort
- internal/daemon: brojevd on unix socket (0600), JSON lines; methods ping/status/tabs/open/close/shutdown; client auto-spawns daemon
- cmd/brojev: daemon/doctor/status/tabs/open/close/stop. Build: go build -o bin/brojev ./cmd/brojev
- Verified headless (scratch BROJEV_HOME): doctor all ok, open data: page 316ms, Wikipedia load 575ms, daemon restart reattaches to same Chrome
- Gotcha fixed: macOS unix socket path max 104 bytes -> long homes fall back to /tmp/brojevd-<uid>-<hash>.sock
- Real ~/.brojev profile not created yet (only scratch tests)

## M1 status (2026-10-12): DONE except emulation + cookies/storage save/restore
- internal/page: lib.js (isolated world per frame, WeakRef stable refs e12 / f2e5, accname-style names, open shadow DOM, clickable-div detection, wrapper dedupe, visible text), page.go (frame tree + OOPIF via setAutoAttach flatten, frame offsets, dialog tracking), snapshot.go (parallel per-frame snapshot, LLM text render), actions.go (click/hover/type/select/key/keytype/scroll/drag/upload/navigate/history/reload/screenshot/eval/wait; actionability: scroll, stable rect, 5-point hit test, refuses covered)
- internal/keyboard: Playwright US layout + macEditingCommands ported (subagent), 14 tests
- internal/browser/downloads.go: allowAndName to ~/.brojev/downloads, renamed to suggested name
- daemon tools.go: all primitives + tabs/tab_new/tab_switch/tab_close/downloads/dialog; action replies include newTabs and optional snapshot
- CLI: brojev <tool> <args> or '<json>'
- Tests: 13 browser fixture tests (BROJEV_E2E=1 go test ./internal/page/) all pass in ~9s: form (text/password masked/select/checkbox/radio/textarea/date/contenteditable), shadow DOM, same-origin + cross-site OOPIF iframe, clickable div, covered refusal, alert/confirm/prompt, upload direct + via chooser, HTML5 DnD + pointer drag, hover menu, keyboard Meta+A/Tab, nested scroll + wheel, navigation/back, stale ref
- Real site: Google Flights full search ZRH->LON one-way Nov 20 driven via CLI primitives; snapshot 30-80ms CLI round trip, ~4.6KB; clicks ~160-210ms
- Popups (target=_blank, window.open) reported as newTabs; download verified

## Findings / gotchas (promote to wiki)
- Chrome 154: --remote-debugging-port propagates to renderers and makes navigator.webdriver=true even headed, even attach-only; Emulation.setAutomationOverride{enabled:false} does not clear it. chrome://inspect toggle route might not (untested).
- Google Flights 2026-10-12 evening: "No results returned. Oops" for brojev AND jev-ultrafast (which passed earlier same day) -> Google/IP side (rate limit?), not brojev
- json/v2 (cdproto jsonv2) is case-sensitive: always tag fields decoded from JS
- cdproto omitzero drops deltaX:0 -> Chrome rejects mouseWheel; send raw params
- macOS unix socket path max 104 bytes
- inline onclick scope: `hidden` resolves to element.hidden (fixture bug, not brojev)
- Per-action 150ms nav grace is a fixed cost; optimize for typing in M2
- Headless default viewport tiny -> --window-size=1280,900

## M1 completion + M2 + M3 (2026-10-12)
- M1 rest: emulate (device presets, viewport/DPR/mobile/touch, geo, tz, locale, color scheme, offline), grant permissions, state save/load (cookies + origin localStorage). 15/15 page tests.
- Attach mode: BROJEV_ATTACH=1 launches without --remote-debugging-port, waits for chrome://inspect toggle (UNTESTED with real toggle: needs user click). BROJEV_PROFILE overrides profile dir.
- M2 Jev loop internal/agent (+ internal/jev client, internal/llm helper): ops CLICK TYPE_TEXT SELECT HOVER DRAG UPLOAD PRESS_KEY SCROLL_* BACK WAIT DIALOG_* DONE BLOCKED; values -> field|value pairs (no helper); joint-confidence gating (2x < 0.2 -> needs_claude); no-progress stop; auto-follow new tabs; expect verification; optional answer extraction. 5 loop tests with fake Jev.
- Live bench eval/tasks.json (15 tasks: wiki, the-internet login/confirm/cancel/upload/hovers/dynamic/keys/dropdown/checkboxes/drag, books x2, HN): 13/15 then 15/15 after images+draggables+DRAG. Mean ~2s/task, whole suite ~$0.008.
  - Wikipedia Godel task: 2.0s (load 195ms, 3 Jev calls 1.07s) vs jev-ultrafast 2.8s claimed / 4.9s measured here
- Bug found+fixed: driven tab must be brought to front (background tabs: input not hit-tested, dialogs deferred) -> Page.bringToFront in use()
- M3: internal/mcpserver (go-sdk v1.8.0), 21 tools, actions return result + fresh snapshot; MCP e2e test passes. Plugin: .claude-plugin/plugin.json + marketplace.json (root, source ./), .mcp.json -> bin/brojev-mcp launcher (builds on first use), skills/brojev/SKILL.md. claude plugin validate passes. Tool names mcp__plugin_brojev_brojev__<tool>.
- Real Claude Code smoke test (claude -p --plugin-dir): books price task answered correctly in 10s / 3 turns.
- README.md, LICENSE (MIT + Playwright Apache-2.0 note)

## M4 (2026-10-12): DONE
- claude_bench.py inside headless Claude Code, 4 tasks x 1 run: brojev 4/4 mean 12.2s, 3.5 turns, $0.66; Playwright MCP 3/4 mean 47.9s, 7.0 turns, $0.75 (miss = omitted stock count)
- Final regression: all unit + e2e suites pass (cdp, keyboard, page 15, agent 5, mcpserver); live bench 15/15 mean 1.94s, $0.0083
- Wiki: ent-brojev, ent-typesafe-jev, src-jev-ultrafast, con-cdp-automation-gotchas (index.md left untouched: it was found empty/truncated in working tree)
- Size: ~6.5k lines Go/JS + 1.3k test lines

## Next step
- user: try BROJEV_ATTACH=1 to see if webdriver flag clears; decide whether to graduate to own repo

## Prior knowledge
- jev-ultrafast live test 2026-10-12: Wikipedia 4.9s pass; Flights 10.3s pass (with future date). Local Chrome via CDP (browser-harness), not cloud.
- Jev API facts from workspace/jev-sniper/NOTES.md: POST api.typesafe.ai/v1/systemone, model jev-latest, 64k ctx, 1200 rpm, $0.042/Mtok input, output free, TS SDK @typesafe-ai/sdk. Weak at numerics.
- Keys: JEV_API_KEY, OPENROUTER_API_KEY

## Open questions
- (decided 2026-10-12: Go; dedicated profile default, see ARCHITECTURE.md Decisions)

## Log
- 2026-10-12: wrote cdp-capability-matrix.md (raw CDP vs Playwright, gotchas, lib rec: TS + ws + devtools-protocol types; real profile via chrome://inspect toggle + DevToolsActivePort, per-connection Allow prompt => long-lived broker)
- 2026-10-12: initiative created (spun off gepa-browser-agent)
- 2026-10-12: wrote study-jev-ultrafast.md (request shape, loop, snapshot, CDP, perf, weaknesses, copy/change list)
