# brojev: raw CDP capability matrix

Date: 2026-10-12. Scope: what it takes to match Playwright (and a human) in Chrome using only the Chrome DevTools Protocol over a WebSocket or pipe. No Playwright, no extension.

Reference sources: CDP docs (chromedevtools.github.io/devtools-protocol, tot), Playwright `packages/playwright-core/src/server/chromium/*` (crPage.ts, crDragDrop.ts, crInput.ts, crNetworkManager.ts), `server/macEditingCommands.ts`, `injected/*`, Puppeteer `packages/puppeteer-core/src/cdp/*`, Chrome blog posts listed at the end.

Notation: methods are `Domain.method`, events are `Domain.event`. "(exp)" means marked experimental in the protocol; it can change between Chrome versions, so pin and test.

## Summary table

| # | Capability | Difficulty |
|---|---|---|
| 1 | Connect to user's logged-in Chrome | med (UX), low (code) |
| 2 | Dedicated profile launch, headless vs headed | low |
| 3 | Targets, tabs, popups, window.open | med |
| 4 | iframes, OOPIF, execution contexts | high |
| 5 | Shadow DOM (open and closed), snapshots | med |
| 6 | Mouse: move, hover, click, dbl, right | low |
| 7 | Drag and drop | med |
| 8 | Wheel, nested scroll containers | low/med |
| 9 | Keyboard, shortcuts, modifiers, IME, rich editors | high |
| 10 | Actionability / auto-wait | med |
| 11 | File upload, file chooser | low |
| 12 | Downloads | low/med |
| 13 | JS dialogs incl. beforeunload | med |
| 14 | HTTP auth, cookies, storage state | low/med |
| 15 | Network interception, idle, inspection | med |
| 16 | Emulation, permissions, focus | low |
| 17 | Canvas / non-DOM UI, screenshots, screencast | med |
| 18 | Clipboard, PDF, select, pickers, dialog, browser UI | med/high |
| 19 | Bot detection surface | med |
| 20 | Client libraries | n/a |

---

## 1. Connecting to the user's existing Chrome (logged-in default profile)

**Playwright:** `chromium.connectOverCDP(endpointURL)` against a Chrome started with `--remote-debugging-port`. `launchPersistentContext(userDataDir)` launches its own Chrome on a directory. Playwright MCP has an "extension" mode to reach the real profile; that is exactly the dependency we are avoiding.

**State of play in 2026:**
- **Chrome 136+:** `--remote-debugging-port` and `--remote-debugging-pipe` are silently ignored when Chrome runs on the default user data dir. No listener, no `DevToolsActivePort` file (reported again on Chrome 151). Rationale is cookie theft by infostealers: a non-default dir gets a different encryption key. Chrome for Testing still honors the flags on any dir.
- **Chrome 144+ sanctioned route:** `chrome://inspect/#remote-debugging`, checkbox "Allow remote debugging for this browser instance". Chrome then shows "Server running at 127.0.0.1:9222" (port may vary) and writes `DevToolsActivePort` into the user data dir root:
  - macOS: `~/Library/Application Support/Google/Chrome/DevToolsActivePort`
  - Linux: `~/.config/google-chrome/DevToolsActivePort`
  - Windows: `%LOCALAPPDATA%\Google\Chrome\User Data\DevToolsActivePort`
  - Line 1 = port, line 2 = `/devtools/browser/<uuid>`. Connect to `ws://127.0.0.1:<port><path>`.
- **HTTP discovery is off on this route:** `/json/version`, `/json/list`, `/json/new` return 404. You must read `DevToolsActivePort` and use Target domain commands (`Target.getTargets`, `Target.createTarget`) instead of `/json/*`.
- **Permission prompt:** every new WebSocket connection triggers a Chrome "Allow remote debugging?" dialog the user must click; an "is being controlled" style infobar/banner shows while attached. Prompt is per connection, not per command.

**CDP to do it:** read `DevToolsActivePort` -> open WS -> `Target.setDiscoverTargets {discover:true}` -> `Target.createTarget {url, newWindow?:true, background?:true}` -> `Target.attachToTarget {targetId, flatten:true}`.

**Gotchas:**
- Architecture consequence: run a long-lived broker (daemon) that holds ONE WebSocket for the whole Chrome session, and have the Claude Code plugin talk to the broker. Reconnecting per tool call means a permission click per call.
- The toggle may need re-enabling after Chrome restarts; detect missing/stale `DevToolsActivePort` (file left behind after crash: verify the port is actually listening) and guide the user.
- Feature labelled beta in M144; re-verify each Chrome major.
- Security: anything local can connect to that port once enabled. Pair with a broker that is the only client.
- Copying the default profile into a custom `--user-data-dir` to "clone the login" is brittle (app-bound encryption on Windows, keychain on macOS) and defeats the intent of the 136 change. Do not build on it.
- Do not apply global side effects to the user's browser: no `Browser.setDownloadBehavior` without restoring, no `Emulation.setDeviceMetricsOverride` on their visible tabs, avoid attaching to all their tabs.
- Work in our own tab or window (`Target.createTarget {newWindow:true}`) so we never hijack the user's active tab.

**Difficulty:** low code, med UX (onboarding flow, permission click, toggle detection).

## 2. Dedicated profile, headless vs headed

**Playwright:** `launchPersistentContext('~/.x/profile', {headless, channel:'chrome'})`, connects over `--remote-debugging-pipe`.

**CDP:** spawn `chrome --user-data-dir=~/.brojev/profile --remote-debugging-port=0 --no-first-run --no-default-browser-check` then read `DevToolsActivePort` for the random port (HTTP `/json/*` works here). Or `--remote-debugging-pipe` (fd 3 write, fd 4 read, NUL-delimited JSON): no open port, safest, what Playwright uses.

Headless: `--headless` is "new headless" (same Chrome binary, full features) since 132; old headless lives on as `chrome-headless-shell`. Headed is needed for human handoff and for the real profile. On the VPS, headed via Xvfb or the existing noVNC desktop lets the user step in.

**Gotchas:**
- Do NOT add `--enable-automation` (sets `navigator.webdriver=true`, adds infobar). Playwright and Puppeteer add it by default.
- Useful flags for a dedicated profile: `--disable-backgrounding-occluded-windows --disable-renderer-backgrounding --disable-background-timer-throttling` (keeps hidden tabs live), `--password-store=basic` on Linux, `--disable-features=Translate,...`. Not available on the attached real profile.
- Headless UA contains `HeadlessChrome`; override with `Emulation.setUserAgentOverride` incl. `userAgentMetadata` or prefer headed.
- User logs in once inside the dedicated profile (headed, via noVNC on VPS); cookies persist there.

**Difficulty:** low.

## 3. Targets and tabs: auto-attach, popups, window.open, closing, switching

**Playwright:** browser-level `Target.setAutoAttach {autoAttach:true, waitForDebuggerOnStart:true, flatten:true}`; per page session the same call again for child targets; emits `popup` from `openerId`. Init is completed before `Runtime.runIfWaitingForDebugger` (crPage.ts notes that setAutoAttach must be sent before runIfWaitingForDebugger).

**CDP:**
- Flatten mode: one WebSocket, every child session addressed by `sessionId` on each message. Always use `flatten:true` (the nested `Target.sendMessageToTarget` mode is deprecated).
- New tabs/popups: `Target.attachedToTarget {sessionId, targetInfo:{targetId,type,url,openerId,openerFrameId}, waitingForDebugger}`. On attach: `Page.enable`, `Page.setLifecycleEventsEnabled`, `Page.addScriptToEvaluateOnNewDocument` (if any), `Target.setAutoAttach` (for its OOPIFs/workers), dialog handlers, then `Runtime.runIfWaitingForDebugger`.
- `Target.targetCreated / targetInfoChanged / targetDestroyed` (needs `setDiscoverTargets`) for tab list and URL tracking.
- Create: `Target.createTarget {url, newWindow, background, browserContextId}`. Close: `Target.closeTarget` (skips beforeunload) or `Page.close` (runs beforeunload). Switch/focus: `Target.activateTarget` or `Page.bringToFront`.
- Isolation: `Target.createBrowserContext` gives an incognito-like context inside the same Chrome (not logged in).

**Gotchas:**
- On the user's real Chrome, browser-level auto-attach attaches to every existing and future tab (dozens). Use `filter` (`[{type:'page', exclude:false}, ...]`) or only attach to targets we create plus targets whose `openerId` is one of ours.
- `noopener` / `rel=noopener` popups have no `openerId`; correlate by timing plus `openerFrameId` absence, or by watching `Page.windowOpen` on the opener session.
- `waitForDebuggerOnStart` deadlocks the popup if you forget `runIfWaitingForDebugger`, including on targets you decide to ignore (call it, then detach).
- Prerender/back-forward cache creates extra `page`-type targets (`subtype: 'prerender'`); filter them.
- Service workers, shared workers, and `tab` type targets also arrive; ignore unless needed.

**Difficulty:** med.

## 4. iframes, OOPIF, cross-origin frames, execution contexts

**Playwright:** tracks frame tree via `Page.frameAttached/frameNavigated/frameDetached` and attaches to OOPIF targets (type `iframe`) through auto-attach; a frame "moves" sessions when it swaps process. Creates a utility world per frame with `Page.createIsolatedWorld` (name `__playwright_utility_world__`) so page JS cannot tamper.

**CDP:**
- Same-process frames: present in `Page.getFrameTree`; contexts via `Runtime.executionContextCreated {context:{id, uniqueId, auxData:{frameId,isDefault,type}}}`.
- OOPIF (cross-site iframes under Site Isolation, which is default on desktop): separate target `type:'iframe'`, own session; that session has its own DOM, Runtime, Page, Network. `Page.frameAttached` fires in parent, then a target attaches whose `targetId == frameId`.
- Context per frame without `Runtime.enable`: `Page.createIsolatedWorld {frameId, worldName, grantUniveralAccess}` returns `executionContextId` directly. Re-create on every `Page.frameNavigated` (contexts die on navigation).
- Cross-frame element addressing: `DOM.getFrameOwner {frameId}` gives the `<iframe>` backendNodeId in the parent; `DOM.describeNode` on an iframe with `pierce` gives `contentDocument` for same-process frames only.

**Gotchas:**
- Coordinates: `DOM.getContentQuads` in an OOPIF session returns coordinates in the iframe's own viewport. Input must be dispatched to the top page session in top-level CSS pixels, so add the offsets of every ancestor iframe's content box (border + padding included, plus any CSS transforms). Playwright does exactly this walk. Easiest robust path: compute in top frame `iframe.getBoundingClientRect()` + `clientLeft/Top` for each ancestor.
- Frames can switch between in-process and OOPIF on navigation; keep frame identity by `frameId`, not by session.
- `executionContextId` is not unique across sessions; key by `(sessionId, contextId)` or use `uniqueId`.
- Sandboxed iframes without `allow-scripts`: isolated world still works.
- Ads and trackers create many OOPIFs; lazy-attach (or skip attaching) frames that are not visible or not needed to keep overhead down.

**Difficulty:** high (this is where most home-grown CDP drivers break).

## 5. Shadow DOM (open and closed), DOM/AX/snapshot piercing

**Playwright:** its CSS and text engines pierce OPEN shadow roots automatically; closed shadow roots are NOT pierced. `getByRole` uses its injected ARIA computation.

**CDP (better than Playwright here):**
- `DOM.getDocument {depth:-1, pierce:true}` returns shadow roots incl. `shadowRootType:'closed'` and user-agent roots, and same-process iframe documents. `DOM.getFlattenedDocument` is deprecated; use getDocument.
- `DOM.querySelector/querySelectorAll` do NOT cross shadow boundaries; walk `shadowRoots` from getDocument, or `DOM.performSearch` (exp) which does search shadow content.
- `DOM.resolveNode {backendNodeId}` turns any node, including inside closed roots, into a RemoteObject you can `Runtime.callFunctionOn`. Page JS cannot reach it but we can.
- `DOMSnapshot.captureSnapshot {computedStyles:[...], includeDOMRects:true, includePaintOrder:true}`: one call, flat arrays of all nodes in the page incl. shadow trees and same-process iframes, with layout bounds, paint order, styles. Fastest way to build an agent-facing "interactive elements" list (milliseconds on big pages). Does NOT include OOPIF content: snapshot each OOPIF session and stitch.
- `Accessibility.getFullAXTree {frameId?, depth?}`: AX tree incl. shadow content; roles, names, states, `backendDOMNodeId`. Per frame, per session for OOPIFs. `Accessibility.queryAXTree {backendNodeId|objectId, accessibleName, role}` for role/name lookup like `getByRole`.

**Gotchas:**
- `Accessibility.enable` is costly on large pages and changes timing; call getFullAXTree on demand rather than keeping it enabled. Linux may need `--force-renderer-accessibility` for full trees in some setups.
- Slots: light-DOM children are shown under the host and as `distributedNodes` in slots; don't double-count.
- `elementFromPoint` hit tests from the document return the shadow host; use `DOM.getNodeForLocation {x,y, includeUserAgentShadowDOM, ignorePointerEventsNone}` from CDP for the real deep target.
- Node ids (`nodeId`) are only valid while DOM agent is tracking; prefer `backendNodeId` as the stable handle.

**Difficulty:** med.

## 6. Mouse: move, hover, click, double, right click

**Playwright (crInput.ts):** `Input.dispatchMouseEvent` mouseMoved (with optional `steps` interpolation), mousePressed, mouseReleased with `clickCount`, `button`, `buttons`, `modifiers`. Click point = center of first content quad after scrolling into view.

**CDP:**
- Hover: `Input.dispatchMouseEvent {type:'mouseMoved', x, y, button:'none'}`.
- Click: mouseMoved -> mousePressed `{button:'left', buttons:1, clickCount:1}` -> mouseReleased `{clickCount:1}`.
- Double: press/release with clickCount 1, then press/release with clickCount 2 (a `dblclick` fires on the second release).
- Right: `button:'right', buttons:2` (fires `contextmenu`).
- Point: `DOM.scrollIntoViewIfNeeded {backendNodeId, rect?}` then `DOM.getContentQuads` (handles transforms, inline-wrapped elements: pick the largest/first visible quad).

**Gotchas:**
- Input coordinates are CSS pixels relative to the top-level viewport of the session's page (see OOPIF note in 4). Not device pixels.
- Mouse state is per page: track current x,y and pressed buttons yourself; `buttons` bitmask must be consistent or pointer events look wrong.
- Synthetic inputs are `isTrusted:true` (they go through the browser input pipeline), so they satisfy user-activation requirements (popups, fullscreen, clipboard, media autoplay).
- Native context menu (right click without page handler) in headed Chrome opens an OS menu that CDP cannot see; send `Escape` key or click elsewhere.
- Teleporting cursor (no moves before press) is a behavioral bot signal; interpolate a few `mouseMoved` steps when stealth matters.
- Input on a background (not focused) tab still works but rendering throttling may delay effects; see 16.

**Difficulty:** low.

## 7. Drag and drop

**Playwright (crDragDrop.ts):** on mouse down + first move it calls `Input.setInterceptDrags {enabled:true}`, waits for `Input.dragIntercepted {data}`, then disables interception and replays `Input.dispatchDragEvent` (`dragEnter`, `dragOver`, `drop`) with that `data` on subsequent moves/up. Pointer-event based libraries (dnd-kit, react-beautiful-dnd, sortable lists, sliders) just need mousePressed + several mouseMoved + mouseReleased.

**CDP:**
- HTML5 native DnD: `Input.setInterceptDrags` (exp) + `Input.dragIntercepted` + `Input.dispatchDragEvent {type:'dragEnter'|'dragOver'|'drop'|'dragCancel', x, y, data:{items:[{mimeType,data}], files?:[paths], dragOperationsMask}, modifiers}`.
- Dropping OS files onto a drop zone (e.g. "drag files here"): `Input.dispatchDragEvent` with `data.files` absolute paths, no interception needed.
- Pointer-event DnD: plain `Input.dispatchMouseEvent` sequence with 5 to 20 intermediate moves and small delays; many libs require a movement threshold and pointer capture.

**Gotchas:**
- Without interception, a native drag in headed Chrome starts an OS-level drag loop and CDP mouse events stop being delivered until it ends. Always intercept for HTML5 DnD.
- Need to know which mode the page uses; cheap heuristic: try intercept; if `dragIntercepted` never fires within ~100 ms of moving, it's pointer-based, keep sending mouse events.
- Some libs check `dataTransfer.types` on dragover only; feed the same `data` on every dragOver.
- Cross-frame drag (source and target in different OOPIFs) is unreliable; Playwright has the same limitation.

**Difficulty:** med.

## 8. Wheel and nested scroll containers

**Playwright:** `mouse.wheel(dx, dy)` -> `Input.dispatchMouseEvent {type:'mouseWheel', x, y, deltaX, deltaY}` at the current mouse position. `scrollIntoViewIfNeeded` for elements.

**CDP:** `Input.dispatchMouseEvent mouseWheel` at a point inside the target scroller (the innermost scrollable under the point receives it, then chains). `Input.synthesizeScrollGesture` (exp) for touch-like flings. Programmatic: `Runtime.callFunctionOn el.scrollBy(...)` or `DOM.scrollIntoViewIfNeeded`.

**Gotchas:**
- Wheel events are async: the scroll is applied on the compositor; wait one or two rAF (or poll `scrollTop`) before reading positions or screenshotting.
- Virtualized lists (react-window, infinite feeds) only render visible rows: scroll in steps and re-snapshot; `scrollIntoView` of an element that is not yet in the DOM is impossible, so the agent needs a "scroll until found" loop.
- Scroll chaining stops at `overscroll-behavior: contain`; position the wheel point inside the correct container.
- Maps/canvas treat wheel as zoom; ctrl+wheel = pinch zoom.

**Difficulty:** low/med.

## 9. Keyboard: typing, shortcuts, modifiers, IME, contenteditable, rich editors

**Playwright (crInput.ts + usKeyboardLayout + macEditingCommands.ts):** for each key: `Input.dispatchKeyEvent {type: text ? 'keyDown' : 'rawKeyDown', key, code, windowsVirtualKeyCode, text, unmodifiedText, location, autoRepeat, modifiers, commands}` then `keyUp`. `keyboard.insertText` -> `Input.insertText`. On macOS it passes `commands` (e.g. `selectAll`, `copy`, `paste`, `deleteBackward`) because editing shortcuts are implemented by the OS text system, not by key events.

**CDP:**
- Modifiers bitmask: Alt=1, Ctrl=2, Meta=4, Shift=8. Send keyDown of the modifier itself first, then the key with `modifiers` set, then keyUps in reverse.
- Printable char: `keyDown` with `text`; non-printable (Enter needs `text:'\r'`, Tab, arrows, Backspace): `rawKeyDown` without text (Enter is the exception: include text `\r` so forms submit).
- Bulk text: `Input.insertText {text}` fires `beforeinput`/`input` but no keydown/keyup; fastest, works for most inputs, contenteditable, Monaco, ProseMirror, Lexical, Slate.
- IME: `Input.imeSetComposition {text, selectionStart, selectionEnd, replacementStart?, replacementEnd?}` (exp) then `Input.insertText` to commit; produces `compositionstart/update/end`.
- Must ship a key table (key, code, keyCode, location, shifted text) like Playwright's `usKeyboardLayout`; copy it (Apache-2.0).

**Gotchas:**
- macOS: Cmd+A/C/V/X/Z via raw key events do nothing in inputs unless `commands:[...]` is supplied. Port Playwright's macEditingCommands map. On Linux/Windows Ctrl shortcuts work natively.
- Browser-level shortcuts (Ctrl+T, Ctrl+W, Ctrl+L, Cmd+Q, F11, Ctrl+Tab) are NOT handled when injected via CDP: input goes to the renderer, not browser UI. Use Target domain equivalents.
- Fields with keydown-driven logic (masked inputs, OTP boxes that auto-advance, autocomplete comboboxes, Google Docs' hidden-iframe editor, games) need real per-key events, not insertText. Policy: insertText by default, per-key when the field has key listeners or when insertText produced no value change.
- `focus` first: click the field or `DOM.focus {backendNodeId}`. In a background tab without focus emulation, the element may not receive focus (16).
- React controlled inputs: never set `.value` directly; use real input events (insertText does this correctly). If you must set value via JS, use the native setter from `HTMLInputElement.prototype` and dispatch `input` + `change`.
- Clearing: select all (with mac `commands`) then `Backspace`, or triple-click then type.
- Dead keys / non-US layouts: CDP has no layout notion; supply `key` and `text` explicitly.

**Difficulty:** high (lots of small tables and per-platform branches).

## 10. Actionability / auto-wait

**Playwright semantics** (docs "Auto-waiting", injectedScript.ts): before an action it retries until all relevant checks pass or timeout:
- Attached: connected to DOM.
- Visible: non-empty bounding box and not `visibility:hidden` (opacity 0 counts as visible).
- Stable: same bounding box for two consecutive animation frames.
- Enabled: not `disabled` (native controls, inside disabled fieldset, `aria-disabled` on ARIA controls).
- Editable: enabled and not `readonly` (for fill).
- Receives events: hit test at the action point lands on the element or a descendant (catches overlays, cookie banners, spinners). On failure: scroll, retry with backoff (0, 20, 100, 100, 500 ms).
- After action: waits for initiated navigations to commit.

**Cheap replication (one round trip):** `Runtime.callFunctionOn {objectId, awaitPromise:true, functionDeclaration}` on the element in an isolated world doing:
1. `el.isConnected`; `el.checkVisibility({checkOpacity:false, checkVisibilityCSS:true})`; rect non-zero.
2. disabled / readonly / aria-disabled checks.
3. `await rAF; r1=rect; await rAF; r2=rect; stable = equal`.
4. `scrollIntoView({block:'center', inline:'center', behavior:'instant'})` if out of viewport, then recompute.
5. Hit test: `let t = document.elementFromPoint(cx, cy)`, descend through `t.shadowRoot?.elementFromPoint` while it returns a host, then `el === t || el.contains(t)` using composed ancestry (`getRootNode().host`).
6. Return `{ok, reason, x, y}` in viewport coordinates.
Then dispatch input. Retry the whole function on failure with backoff, up to the action timeout. Use `DOM.getNodeForLocation` as a cross-check when the element is inside an OOPIF (JS hit test cannot see across process boundaries; do the check inside the iframe's session with iframe-local coordinates).

**Gotchas:**
- `checkVisibility` needs Chrome 105+; fine.
- Stability: CSS transitions longer than two frames pass the check mid-flight in rare cases; acceptable (Playwright has the same).
- Overlays that disappear on their own (toasts) vs blocking modals: report the intercepting element in the failure reason so the agent can dismiss it.
- Waiting for navigation after click: watch `Page.frameStartedLoading`/`Page.frameRequestedNavigation` on the main frame within ~100 ms of the click; if one starts, wait for `Page.lifecycleEvent name=DOMContentLoaded` (or `load`) for the new `loaderId`.

**Difficulty:** med.

## 11. File upload

**Playwright:** `setInputFiles` -> `DOM.setFileInputFiles {files, backendNodeId}` (for large/remote files streams them). `page.on('filechooser')` -> `Page.setInterceptFileChooserDialog {enabled:true}` + `Page.fileChooserOpened`.

**CDP:**
- Direct: `DOM.setFileInputFiles {files:[absPaths], backendNodeId}` on the `<input type=file>` (works for hidden inputs too; fires input/change).
- When the page opens the chooser itself (button that calls `input.click()`): `Page.setInterceptFileChooserDialog {enabled:true, cancel?:false}` then on `Page.fileChooserOpened {frameId, mode:'selectSingle'|'selectMultiple', backendNodeId}` call `DOM.setFileInputFiles` on that `backendNodeId`.

**Gotchas:**
- Paths must exist on the machine running Chrome (matters if Chrome is remote or in Docker).
- File System Access API (`window.showOpenFilePicker`, `showDirectoryPicker`) is NOT intercepted and opens a native OS dialog. Fallback: inject an init script that shims `showOpenFilePicker` to return handles built from a hidden `<input type=file>` we control, or human/OS automation.
- Interception is per page session; enable it on every new tab/popup.
- `webkitdirectory` inputs: pass a directory path.
- Drop-zone uploads: use `Input.dispatchDragEvent` with `data.files` (section 7).

**Difficulty:** low.

## 12. Downloads

**Playwright:** `Browser.setDownloadBehavior {behavior:'allowAndName', downloadPath: tmp, eventsEnabled:true, browserContextId}`; maps `Browser.downloadWillBegin` + `Browser.downloadProgress` to a Download object; file saved under the GUID name and renamed on `saveAs`.

**CDP:** same: `Browser.setDownloadBehavior {behavior:'allow'|'allowAndName'|'deny'|'default', downloadPath, eventsEnabled:true}`; events `Browser.downloadWillBegin {frameId, guid, url, suggestedFilename}` and `Browser.downloadProgress {guid, totalBytes, receivedBytes, state:'inProgress'|'completed'|'canceled', filePath?}`. Cancel: `Browser.cancelDownload {guid}`. (`Page.setDownloadBehavior` is deprecated.)

**Gotchas:**
- On the user's real profile this overrides their download settings globally; scope with `browserContextId` where possible and restore `behavior:'default'` on detach. Crash = left overridden, so prefer to set it only during a download-expecting action.
- With `default` behavior and "Ask where to save each file" on, a native Save dialog appears that CDP cannot drive.
- "Download multiple files" permission bubble on repeated downloads: grant `automaticDownloads` via `Browser.setPermission`.
- `totalBytes` is 0 for chunked responses; rely on `state`.
- Downloads triggered from popups or in OOPIFs still report through the browser session (good: listen on the root connection).

**Difficulty:** low/med.

## 13. JS dialogs: alert, confirm, prompt, beforeunload

**Playwright:** auto-dismisses unless a `dialog` listener exists. `Page.javascriptDialogOpening` -> `Page.handleJavaScriptDialog {accept, promptText}`.

**CDP:** `Page.enable` on every page session. Event `Page.javascriptDialogOpening {url, frameId, message, type:'alert'|'confirm'|'prompt'|'beforeunload', hasBrowserHandler, defaultPrompt}`. Respond `Page.handleJavaScriptDialog {accept, promptText}`. `Page.javascriptDialogClosed` confirms.

**Gotchas:**
- While a dialog is open the renderer main thread is blocked: any in-flight `Runtime.evaluate`/`callFunctionOn` hangs and input is not processed. The broker must handle dialogs out of band (event handler, never awaited behind a pending eval) and give every command a timeout.
- Dialogs from OOPIFs arrive on that iframe's session; enable Page there too.
- beforeunload only shows if the page got a user gesture (sticky activation). `Target.closeTarget` skips it; `Page.close` runs it; `Page.navigate` triggers it too. Policy: auto-accept beforeunload unless the agent asked otherwise (it's usually "unsaved changes").
- Real-profile headed mode: the user also sees the dialog; whoever answers first wins.
- Policy for brojev: surface alert/confirm/prompt to the agent with message text; default timeout auto-dismiss to avoid deadlock.

**Difficulty:** med (because of the blocking semantics, not the API).

## 14. Auth: HTTP basic/digest, cookies, storage state

**Playwright:** `httpCredentials` -> `Fetch.enable {handleAuthRequests:true, patterns:[{urlPattern:'*'}]}` + `Fetch.authRequired` -> `Fetch.continueWithAuth {authChallengeResponse:{response:'ProvideCredentials', username, password}}`. `storageState()` = cookies + localStorage per origin (+ optional IndexedDB since 1.51); restore by `Storage.setCookies` and an init script that writes localStorage on first visit per origin.

**CDP:**
- HTTP auth: as above. Must also continue all paused requests (`Fetch.requestPaused` -> `Fetch.continueRequest`) because enabling Fetch pauses everything matching the pattern. Use a narrow pattern or `requestStage:'Request'` and continue immediately.
- Cookies: `Storage.getCookies {browserContextId?}` / `Storage.setCookies` / `Storage.clearCookies` (browser-wide); `Network.getCookies {urls}` for per-page. `Network.setCookie` for one.
- localStorage/sessionStorage: `DOMStorage.enable` + `DOMStorage.getDOMStorageItems {storageId:{storageKey|securityOrigin, isLocalStorage}}` (requires that origin to be loaded in some frame) or evaluate `Object.entries(localStorage)` in the origin.
- IndexedDB: `IndexedDB.requestDatabaseNames / requestData` (read), writes via page JS.
- Restore before page scripts run: `Page.addScriptToEvaluateOnNewDocument` that seeds localStorage when `location.origin` matches, or navigate to origin blank page, seed, then navigate.

**Gotchas:**
- On the real profile, storage state is already there: this matters mainly for the dedicated/headless profile and for exporting a session to a VPS Chrome.
- Fetch auth: if `Fetch.enable` is on, Chrome's native auth popup is suppressed; if it's off and a site sends 401 + WWW-Authenticate, a native browser-UI auth prompt appears that CDP cannot fill. Enable `handleAuthRequests` lazily on 401 is too late for that request: keep Fetch on with a narrow pattern for known basic-auth hosts.
- Partitioned (CHIPS) cookies have `partitionKey`; preserve it on export/import.
- `Network.getAllCookies` is deprecated; use Storage.

**Difficulty:** low/med.

## 15. Network: interception, idle, inspection

**Playwright:** `page.route` -> `Fetch.enable` + `Fetch.requestPaused` -> `continueRequest` / `fulfillRequest` / `failRequest`; inspection from `Network.requestWillBeSent / responseReceived / loadingFinished / loadingFailed` (+ `requestWillBeSentExtraInfo/responseReceivedExtraInfo` for raw headers and cookies). `waitForLoadState('networkidle')` = no network activity for 500 ms.

**CDP:**
- Intercept: `Fetch.enable {patterns:[{urlPattern, resourceType, requestStage:'Request'|'Response'}]}`; on `Fetch.requestPaused`: `Fetch.continueRequest {url?, method?, postData?, headers?}`, `Fetch.fulfillRequest {responseCode, responseHeaders, body(base64)}`, `Fetch.failRequest {errorReason}`, at response stage `Fetch.getResponseBody` and `Fetch.continueResponse`.
- Blocking only: `Network.setBlockedURLs` (cheap, no pause).
- Inspect: `Network.enable {maxTotalBufferSize, maxResourceBufferSize}`; `Network.getResponseBody {requestId}` after `loadingFinished`; `Network.getRequestPostData`.
- Idle: `Page.setLifecycleEventsEnabled {enabled:true}` -> `Page.lifecycleEvent {frameId, loaderId, name:'networkAlmostIdle'|'networkIdle'|'DOMContentLoaded'|'load'|'firstMeaningfulPaint'...}`. networkIdle = 0 connections for 500 ms, networkAlmostIdle = at most 2. Or count in-flight requests yourself from Network events (works for SPA actions after load).
- WebSockets: `Network.webSocketCreated / webSocketFrameSent / webSocketFrameReceived`.

**Gotchas:**
- Every paused request must be answered or the page hangs. Keep the handler synchronous and fast; never await model calls inside it.
- Lifecycle `networkIdle` only fires once per navigation (per loaderId); SPA route changes (`Page.navigatedWithinDocument`) do not fire it. For "wait until settled after click" implement own in-flight counter with a quiet window (e.g. 300 to 500 ms, ignore websockets, EventSource, long-poll, and analytics beacons).
- Long-polling/streaming pages never reach idle: always cap with a timeout and fall back to DOM stability (MutationObserver quiet period).
- Response bodies get evicted from the buffer; fetch body right after `loadingFinished` if you need it. Bodies of redirects/preflights are absent.
- Service-worker-served responses: page Fetch does not see SW-handled requests the same way; `Network.setBypassServiceWorker {bypass:true}` when intercepting.
- Enabling Network on the real profile's tabs adds overhead; enable only on our tabs.

**Difficulty:** med.

## 16. Emulation, permissions, background-tab focus

**Playwright:** per context: `Emulation.setDeviceMetricsOverride`, `setUserAgentOverride`, `setTouchEmulationEnabled`, `setEmulatedMedia`, `setTimezoneOverride`, `setLocaleOverride`, `setGeolocationOverride`, `Browser.grantPermissions`, and in headless/multi-page always `Emulation.setFocusEmulationEnabled {enabled:true}` (crPage.ts).

**CDP:**
- Viewport/device: `Emulation.setDeviceMetricsOverride {width, height, deviceScaleFactor, mobile, screenOrientation}` + `Emulation.setUserAgentOverride {userAgent, platform, userAgentMetadata}` + `Emulation.setTouchEmulationEnabled {enabled, maxTouchPoints}`; device presets = copy Playwright's deviceDescriptorsSource.json.
- `Emulation.setGeolocationOverride {latitude, longitude, accuracy}` + permission `geolocation`.
- `Emulation.setTimezoneOverride {timezoneId}`, `Emulation.setLocaleOverride {locale}`, `Emulation.setEmulatedMedia {media, features:[{name:'prefers-color-scheme', value:'dark'}]}`.
- Permissions: `Browser.grantPermissions {permissions:['geolocation','notifications','clipboardReadWrite','clipboardSanitizedWrite','camera','microphone',...], origin, browserContextId}`; finer: `Browser.setPermission {permission:{name}, setting:'granted'|'denied'|'prompt', origin}`; `Browser.resetPermissions`.
- Focus: `Emulation.setFocusEmulationEnabled {enabled:true}` makes `document.hasFocus()` true and focus/blur work in a non-focused tab/window.
- Network/CPU throttling: `Network.emulateNetworkConditions`, `Emulation.setCPUThrottlingRate`.
- Touch input: `Input.dispatchTouchEvent`.

**Gotchas:**
- Background tabs in headed Chrome throttle timers (1/min after 5 min hidden), pause rAF, and `Page.captureScreenshot` may stall or return stale frames. Focus emulation fixes focus but not rendering throttling. Options: `Page.bringToFront` before screenshots (steals the user's focus on the real profile), run our tab in its own window (an unoccluded window is not throttled), or use the dedicated profile with the anti-backgrounding flags.
- Viewport override on a headed real-profile window desyncs page size from the window; avoid on the real profile, prefer `Browser.setWindowBounds {windowId, bounds}` (`Browser.getWindowForTarget` first).
- Permission grants are scoped to origin/context; grant on the real profile persists only for the CDP session but don't spray grants there.

**Difficulty:** low.

## 17. Canvas and non-DOM UIs (maps, games, Figma-like), screenshots, screencast

**Playwright:** `page.screenshot` -> `Page.captureScreenshot`; `mouse.click(x,y)` on coordinates; no special canvas support. Video recording via `Page.startScreencast`.

**CDP:**
- `Page.captureScreenshot {format:'jpeg'|'webp'|'png', quality, clip:{x,y,width,height,scale}, captureBeyondViewport:false, fromSurface:true, optimizeForSpeed:true}`. JPEG/WebP q 60 to 80 with `optimizeForSpeed` is typically ~20 to 80 ms for a 1280x800 viewport on desktop hardware; PNG of a retina viewport can take 150 to 400 ms. Use `clip.scale` (e.g. 0.5) to shrink for the model.
- `Page.startScreencast {format:'jpeg', quality, maxWidth, maxHeight, everyNthFrame}` -> `Page.screencastFrame {data, metadata, sessionId}` -> must `Page.screencastFrameAck {sessionId}`. Emits only on repaint: a cheap "did the screen change" signal and a near-zero-latency last frame.
- Coordinate click: `Input.dispatchMouseEvent` at CSS px; drag via mouse sequence; keyboard as usual.
- Canvas pixel read: `Runtime.callFunctionOn canvas.toDataURL()` (fails on tainted canvases; WebGL with `preserveDrawingBuffer:false` returns blank), so screenshot is the general path.
- Deterministic frames: `HeadlessExperimental.beginFrame` only in chrome-headless-shell with special flags; not worth it.

**Gotchas:**
- DPR: screenshots are in device pixels (2x on Retina) unless `clip.scale` adjusts; inputs are CSS pixels. Always return the CSS-px mapping (scale factor) with the image to the model.
- `captureBeyondViewport:true` (full page) triggers a relayout/resize, can break sticky headers and fires resize handlers; avoid in the hot loop.
- Background tab screenshots: see 16.
- For the agent: use DOM/AX snapshot first, screenshot plus set-of-marks overlay (numbered boxes drawn from DOMSnapshot rects) for visual tasks, raw coordinates only for canvas.
- Figma/Google Sheets/Maps also expose some AX tree content (hidden accessibility layers); check AX before going pixel.

**Difficulty:** med.

## 18. Clipboard, PDF, select, pickers, `<dialog>`, permission bubbles, auth popups, browser UI

| Item | Playwright | CDP approach | Gotchas | Diff |
|---|---|---|---|---|
| Clipboard read/write | `grantPermissions(['clipboard-read','clipboard-write'])` + page `navigator.clipboard` | `Browser.grantPermissions {permissions:['clipboardReadWrite','clipboardSanitizedWrite']}` + `Emulation.setFocusEmulationEnabled` + `Runtime.evaluate navigator.clipboard.readText()` with `userGesture:true, awaitPromise:true`. Paste into a field: Ctrl/Cmd+V key events (mac needs `commands:['paste']`) or just `Input.insertText` | Clipboard API requires focused document; reads the real OS clipboard (on real profile that's the user's clipboard, be careful). No CDP command to set OS clipboard directly; write via page JS | low/med |
| Print to PDF | `page.pdf()` | `Page.printToPDF {printBackground, landscape, paperWidth, paperHeight, marginTop..., pageRanges, preferCSSPageSize, transferMode:'ReturnAsStream'}` then `IO.read` / `IO.close` | Historically headless only; recent Chrome versions accept it headed too, verify on target version. `window.print()` from the page opens native print preview: stub `window.print` via init script | low |
| Native `<select>` | `selectOption` sets `option.selected` via JS and dispatches `input`+`change` | Same via `Runtime.callFunctionOn`. Or focus + keyboard (type option text, Enter) which works with the popup closed | Clicking a `<select>` in headed Chrome opens a popup widget (separate native/popup widget, not in the page DOM, not in screenshots on some platforms). Never click-to-open; set value or use keys. Custom (div-based) dropdowns are normal DOM | low |
| Date / time / month inputs | `fill('2026-10-12')` sets value + events | Set `value` in ISO format via native setter + `input`/`change`, or focus and type segment-by-segment with keys | Native picker popup is browser UI. Custom JS date pickers (react-datepicker, MUI) are DOM; agent clicks them. Locale display differs from value format | low |
| Color input | `fill('#ff0000')` | Set value + `input`/`change` | Native color chooser is OS UI | low |
| `<dialog>` element, popovers | Normal DOM (top layer) | Normal DOM; hit test works; `showModal` makes rest inert, so actionability "receives events" correctly fails for background elements | `inert` content fails clicks: agent must close dialog first | low |
| Permission bubbles (geo, notifications, camera, mic, MIDI, clipboard, storage access, local network) | Pre-grant via `grantPermissions` | `Browser.grantPermissions` / `Browser.setPermission` before the page asks | Bubble already shown cannot be clicked via CDP; grant then reload or re-trigger. Camera/mic real devices: in dedicated profile use `--use-fake-device-for-media-stream --use-fake-ui-for-media-stream` | low |
| HTTP auth popup | `httpCredentials` | `Fetch.authRequired` (section 14) | Must have Fetch enabled before the 401 | low |
| FedCM "Sign in with Google" account chooser | n/a | `FedCm.enable` + `FedCm.dialogShown` -> `FedCm.selectAccount` / `FedCm.clickDialogButton` / `FedCm.dismissDialog` | This browser UI IS scriptable via CDP | med |
| WebAuthn / passkeys | `virtualAuthenticator` (via CDP) | `WebAuthn.enable` + `WebAuthn.addVirtualAuthenticator` | Virtual authenticator only works for credentials created with it; real passkeys (Touch ID, phone, security key) need the human | med |
| Bluetooth device chooser | n/a | `DeviceAccess.enable` + `DeviceAccess.deviceRequestPrompted` -> `selectPrompt` / `cancelPrompt` (exp) | WebUSB/WebHID/Serial choosers have no CDP equivalent | med |
| Autofill / password manager UI | n/a | `Autofill.trigger` (exp, cards/addresses for testing) | Password manager bubbles, autofill dropdowns are browser UI; in the real profile rely on page already being logged in. Disable password bubble in dedicated profile via prefs | high |
| Native context menu, extension popups, omnibox, tab strip, translate bar, "open in app" protocol prompt, Save As dialog | none (Playwright can't either) | Avoid triggering; Target domain for tab ops; `Escape` key; prefs/flags in dedicated profile | Fallback: OS-level automation or human | high |

## 19. Captchas and bot detection

**What CDP-driven Chrome reveals:**
- `navigator.webdriver === true`: set by `--enable-automation` and by headless mode (and some versions tie it to `--remote-debugging-pipe`); NOT set merely by attaching over CDP to a normally launched headed Chrome (chrome://inspect route or plain `--remote-debugging-port`). Verify on each target version.
- "Chrome is being controlled by automated test software" infobar (`--enable-automation`).
- `Runtime.enable` side effects: the classic probe (console.log an Error whose `stack` getter flips a flag when CDP serializes it) was killed by V8 changes in May 2025 ("Avoid error side effects in DevTools", "Apply getter guard throughout error preview"). Other residual signals remain (console serialization of other objects, timing of `executionContextCreated`, source URLs `__puppeteer_evaluation_script__` / `//# sourceURL=` markers in stack traces from injected code).
- Headless fingerprints: `HeadlessChrome` UA, missing plugins/codecs in shell, WebGL renderer "SwiftShader", screen size 800x600 defaults, client hints mismatching overridden UA.
- Behavioral: instant cursor teleports, zero-delay typing, no scroll before click, perfect center clicks.
- Remote debugging port itself can be probed by local software, not by websites (CORS/private network access blocks pages from hitting localhost:9222 in modern Chrome, but do not rely on it).

**Legitimate mitigations (bias: look like the user, because it is the user):**
- Use the real profile (or a long-lived dedicated profile), headed, without `--enable-automation`. Real cookies and history pass most risk scoring and Cloudflare Turnstile non-interactive challenges.
- Do not call `Runtime.enable` on page sessions; get contexts via `Page.createIsolatedWorld` and evaluate there (this is the patchright/rebrowser approach and also avoids page JS seeing our globals). Avoid `sourceURL` tags in injected scripts; keep page-world injection to zero where possible.
- Do not override UA/client hints on real Chrome; if you must, set `userAgentMetadata` consistently.
- Human-like input pacing: a few interpolated mouse moves before click, small jitter inside the element, 20 to 80 ms per-key delays only on sensitive pages (keep fast path elsewhere).
- Captchas: detect (iframe src matching recaptcha/hcaptcha/challenges.cloudflare.com, AX names like "I'm not a robot") and hand off to the human (Telegram notify plus noVNC link, or the user's own screen in real-profile mode), then resume. Do not integrate captcha-solving farms; it is against the ToS of most sites and out of scope.

**Difficulty:** med.

## 20. Lightweight CDP client libraries

| Library | Lang | Size / deps | Maintenance (as of 2026-10) | Notes |
|---|---|---|---|---|
| raw `ws` + `devtools-protocol` (types only) | TS/Node | ws 150 KB, 0 deps; devtools-protocol 3.7 MB but type-only (no runtime cost if `import type`) | ws 8.22 active; devtools-protocol published from Chromium continuously (0.0.1716261) | Client is ~150 to 250 lines: id counter, pending map, `sessionId` routing for flatten mode, event emitter keyed by `sessionId:method`, per-command timeout. Full control, zero magic. Node 22+ also has a global `WebSocket` (undici) so even ws is optional |
| chrome-remote-interface 0.34 | TS/Node (JS + d.ts) | 2.0 MB unpacked (bundled protocol JSON), deps ws + commander | single maintainer, last push Feb 2026, 4.5k stars | Mature, simple, supports `sessionId` on send. Dynamic domain proxies are convenient but its default HTTP discovery (`/json/*`) breaks on the chrome://inspect route (404); connect by explicit WS URL |
| puppeteer-core 25 | TS | 6 MB + chromium-bidi, devtools-protocol, @puppeteer/browsers | Google, very active | Too heavy to be "no framework" but the best reference implementation (cdp/ dir) to copy patterns from: target manager, frame manager, isolated worlds, input. Could be an internal fallback, but it adds `--enable-automation` defaults only when it launches; `connect()` does not |
| cdp-use (npm 0.0.11) | TS | 1.5 MB, dep ws | early, last publish Dec 2025 | TS sibling of the Python one; young, low adoption |
| cdp-use 1.4.5 | Python 3.11+ | ~350 KB wheel, deps websockets, httpx, typing-extensions | browser-use team, repo active (Jul 2026), ~320 stars | Generated typed bindings (`cdp.send.Page.navigate(...)`, `cdp.register.Page.loadEventFired(cb)`), session_id per call. This is what browser-use moved to when it dropped Playwright (2025); browser-use's `browser/session.py` + `watchdogs/` (one watchdog per concern: downloads, dialogs, popups, crashes, security) is a good architecture reference |
| python-cdp 0.3 / pycdp | Python | small | HMaker fork, last push Oct 2025; original pycdp dead since 2023 | Typed, sans-IO bindings; less active |
| nodriver 0.50 / zendriver 0.17 | Python | ~380 KB, websockets + mss etc. | both active (Oct 2026) | Full frameworks with stealth defaults (no webdriver flag, no Runtime.enable); opinionated, larger API surface than we want, but good reading for stealth details |
| pychrome | Python | tiny | dead (2023) | skip |

**Recommendation:** TypeScript/Node with a hand-written ~200-line flatten-mode client on `ws` (or Node's global WebSocket) plus `import type` from `devtools-protocol` for typed commands/events. Reasons: Claude Code plugins/MCP servers are Node-native and start fast; zero runtime deps beyond ws; full control over sessions, timeouts, and the no-Runtime.enable policy; the chrome://inspect route needs explicit WS-URL connection anyway, which removes most of what chrome-remote-interface adds. Copy specific tables/algorithms (Apache-2.0, keep notices) from Playwright: `usKeyboardLayout`, `macEditingCommands`, device descriptors, crDragDrop flow, OOPIF quad translation. Use puppeteer-core source as the reference for target/frame management. If the team prefers Python, use cdp-use and model the runtime on browser-use's watchdog design.

---

## Things CDP cannot do that a human can, and the proposed fallback

1. **Native OS dialogs** (Save As when "ask where to save" is on, print preview, File System Access pickers, OS file picker if not intercepted). Fallback: avoid (download behavior override, file chooser interception, `window.print` and `showOpenFilePicker` shims); else OS automation (computer-use MCP on macOS, xdotool on the VPS Xvfb) or human handoff.
2. **Browser chrome UI** (omnibox, tab strip drag, extension popups, translate/password/autofill bubbles, native context menu, `<select>`/date/color popups). Fallback: use Target/Browser domain equivalents and value-setting via JS; dismiss with Escape; disable via prefs in the dedicated profile; OS automation as last resort.
3. **Permission bubbles already on screen.** Fallback: pre-grant with `Browser.setPermission`, then reload/re-trigger.
4. **Captchas and interactive bot challenges.** Fallback: detect and hand off to the human via Telegram notify plus noVNC/live screen; resume after the challenge clears.
5. **Real 2FA** (SMS, authenticator app, hardware key, platform passkey with biometrics). Fallback: human handoff; persistent logged-in profile to minimize prompts; WebAuthn virtual authenticator only for sites/tests we control.
6. **Device choosers for WebUSB/WebHID/Serial, screen-share picker (`getDisplayMedia`).** Fallback: dedicated-profile flags (`--auto-select-desktop-capture-source=...`, `--use-fake-ui-for-media-stream`); otherwise human.
7. **Browser-level keyboard shortcuts** (Ctrl+T/W/L/Tab, F11, Cmd+Q). Fallback: `Target.createTarget/closeTarget/activateTarget`, `Browser.setWindowBounds {windowState:'fullscreen'}`.
8. **Reading pixels of background/occluded tabs reliably and acting without stealing focus on the real profile.** Fallback: own window for brojev tabs, focus emulation, `Page.bringToFront` only when needed, or dedicated profile with anti-throttling flags.
9. **OS-level clipboard and cross-app actions** (copy from another app, drag a file from Finder). Fallback: page `navigator.clipboard` with permission; `Input.dispatchDragEvent` with `data.files` for file drops; computer-use for anything outside Chrome.
10. **Judging visuals with human intuition** (is this the right product photo, is the layout broken). Fallback: screenshot to the model; escalate ambiguous decisions to the user.
11. **Connecting to the real profile without a one-time click.** Fallback: none by design (Chrome 136+/144+ security); keep a single long-lived broker connection so the user clicks Allow once per Chrome session.

## Sources

- Chrome blog, "Changes to remote debugging switches to improve security" (Chrome 136): https://developer.chrome.com/blog/remote-debugging-port
- Chrome blog, "Let your Coding Agent debug your browser session with Chrome DevTools MCP" (M144 autoConnect, chrome://inspect toggle, permission dialog): https://developer.chrome.com/blog/chrome-devtools-mcp-debug-your-browser-session
- trycua/cua issue 2916 (Chrome 151: default-profile flag ignored, DevToolsActivePort format, /json 404 on toggle route): https://github.com/trycua/cua/issues/2916
- miniasp blog on autoConnect vs 9222 (per-connection Allow dialog, /json/version 404): https://blog.miniasp.com/post/2026/08/16/chrome-remote-debugging-autoconnect-9222-cdp
- Castle.io on the Runtime.enable/Error.stack signal dying in 2025: https://blog.castle.io/why-a-classic-cdp-bot-detection-signal-suddenly-stopped-working-and-nobody-noticed/
- DataDome on CDP detection: https://datadome.co/threat-research/how-new-headless-chrome-the-cdp-signal-are-impacting-bot-detection/
- cdp-use on PyPI: https://pypi.org/project/cdp-use/
- CDP reference: https://chromedevtools.github.io/devtools-protocol/tot/
- Playwright source verified for this doc: crPage.ts (setAutoAttach flatten + waitForDebuggerOnStart, createIsolatedWorld, setFocusEmulationEnabled, runIfWaitingForDebugger ordering), crDragDrop.ts (setInterceptDrags, dragIntercepted, dispatchDragEvent), macEditingCommands.ts.
- npm/PyPI registry metadata pulled 2026-10-12 for sizes and versions.
