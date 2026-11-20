# Spike: Jev clicks the Turnstile "Verify you are human" checkbox

Date: 2026-11-20
Branch: `spike/jev-turnstile-click`, off `feat/turnstile-detector-resolver`
Status: complete, 10/10

## Question

Can Jev locate the managed Turnstile checkbox from a screenshot alone, and does
a real CDP click at the point it returns produce a token? This covers the
checkbox path only. No image grid, no CapSolver.

## Setup

- Cloudflare test sitekey `3x00000000000000000000FF` (always interactive) on a
  local fixture page.
- Headed Chrome on a throwaway profile, launched through `chrome.Ensure`, driven
  through `internal/page`.
- One System One request per run with four questions against one image:
  - `box`: the square checkbox left of "Verify you are human"
  - `box`: the entire verification widget
  - `noul`: is the checkbox visible
  - `choice`: widget state (checkbox, spinner, verified, image challenge, other)
- Image sent in `state` as a PNG asset, with `TypeSafe-Danger-Zone-Images: true`.
- Screenshot is a clipped region around the widget, 460x148 CSS px at scale 2,
  producing a 920x296 PNG. It stays inside Jev's 256 to 1568 px limits.
- Click at the checkbox box center through `Page.ClickXY`, then poll
  `input[name="cf-turnstile-response"]`.
- Ten fresh page loads, one run each.

## Results

| run | box conf | box left,top,right,bottom | click (x, y) | token before | token after |
| --- | --- | --- | --- | --- | --- |
| 1 | 0.83 | 0.12, 0.44, 0.19, 0.56 | 87.3, 188.7 | no | yes |
| 2 | 0.81 | 0.12, 0.43, 0.19, 0.56 | 87.3, 188.0 | no | yes |
| 3 | 0.82 | 0.12, 0.44, 0.19, 0.56 | 87.3, 188.7 | no | yes |
| 4 | 0.82 | 0.12, 0.44, 0.20, 0.57 | 89.6, 189.5 | no | yes |
| 5 | 0.84 | 0.12, 0.43, 0.19, 0.55 | 87.3, 187.3 | no | yes |
| 6 | 0.83 | 0.12, 0.44, 0.19, 0.57 | 87.3, 189.5 | no | yes |
| 7 | 0.78 | 0.12, 0.43, 0.20, 0.56 | 89.6, 188.0 | no | yes |
| 8 | 0.84 | 0.12, 0.43, 0.19, 0.55 | 87.3, 187.3 | no | yes |
| 9 | 0.80 | 0.12, 0.43, 0.19, 0.55 | 87.3, 187.3 | no | yes |
| 10 | 0.81 | 0.12, 0.43, 0.19, 0.55 | 87.3, 187.3 | no | yes |

Summary:

- Token after click: 10/10. Token was never present before the click.
- Checkbox present noul: 0.99 every run. Widget state choice: `checkbox` every run.
- Box confidence: 0.78 to 0.84, mean 0.82.
- Jev latency: 221 to 564 ms, mean 289 ms.
- Input tokens: 1248 to 1252 per run, mean 1250.
- Cost: $0.0000525 per run, $0.000525 for all ten, using $0.042/M input.
- Model: `jev-1.14.0`.
- Token appeared on the first poll after the click, 0 ms wait, every run.

The predicted crops show the box covers the checkbox square plus the first
letters of the label. The click at the box center still works because the left
area of the widget is one clickable target. The box is stable but loose, not a
precise checkbox rect.

## What this means for the approved design

1. On this test key the managed widget renders in a **closed shadow root** with
   **no iframe at all**. The light DOM is only
   `<div><div></div><input hidden name="cf-turnstile-response"></div>`, with
   `firstElementChild.shadowRoot === null`, zero iframes, and zero open shadow
   roots. `Page.getFrameTree` reports only the main frame.

   The design's mode heuristic, "managed when an interactive widget frame is
   visible, invisible when the root exists with no visible frame", will classify
   this widget as invisible or unknown. Its in-page step will also find no frame
   to attach to and no checkbox element to click. The e2e case for `3x` will hit
   this.

   Fix for the detector: treat a widget root with a visible rendered size (for
   example at least 100x30 CSS px) as managed, regardless of frame presence.
   Treat a frame as optional. The AX tree may still expose "Verify you are
   human" even when the shadow root is closed, but that is untested here.

2. The fallback blind click at the widget's left-center is the only deterministic
   path for this widget, and Jev vision is a stronger version of it. The
   design's DOM checkbox click does not apply when the shadow root is closed.

3. The widget host is full width (736 CSS px here), while the visual widget is
   about 300x65. Crop to the visual area, not the host rect, or the screenshot
   exceeds the 1568 px maximum after scaling.

4. Do not treat the box as a tight checkbox rect. Confidence is honest but the
   box includes part of the label. The center click is fine; a future tight crop
   would need a stricter prompt or a post-crop refinement.

5. Cost and latency fit the 60 s budget easily. Jev at about $0.00005 per solve is
   roughly 20x cheaper than the CapSolver list price of $0.0012.

6. Images remain Dreamer-ready. Keep any production path behind a flag, and keep
   CapSolver as the token fallback.

## Limits

- Only the `3x` test key. Production widgets may differ in frame layout, theme,
  size, language, and risk behavior.
- Fresh profile, headed Chrome, one network location. No risk-score variation.
- Box accuracy is judged from saved predicted crops, not a ground-truth element
  rect, because no checkbox element is reachable in the closed shadow root.
- The test key issued the token instantly and locally. Real tokens may take
  longer and may fail after a correct click if the environment is distrusted.
- No image-grid challenge and no full-page interstitial coverage.

## Recommendation

Worth building as an optional in-page visual fallback for the managed checkbox,
after the deterministic click and before CapSolver:

1. Detect a visible widget root by rendered rect, not by frame presence.
2. Clip and scale a screenshot to the 256 to 1568 px window.
3. One Jev request: checkbox box, noul present, state choice.
4. Gate on noul >= 0.5 and box confidence >= 0.5 (observed minimum 0.78).
5. Click the box center with real CDP input, poll the response token.
6. On unresolved, fall through to CapSolver.

Default the visual phase off (`TURNSTILE_VISUAL=jev`) until it has run against
real widgets.

## Reproduction

```sh
set -a; . ~/.brojev/env; set +a
go run ./spike/jev-click -runs 10
```

Outputs `runNN.png` and `runNN-predicted.png` per run in a temp directory. The
spike prints one JSON record per run and a summary line.
