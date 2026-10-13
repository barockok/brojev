---
name: setup
description: First-time setup and health check for brojev - checks Chrome and the daemon, gets the API key configured, picks the Chrome profile (dedicated or the user's real Chrome), and runs a smoke test. Use when the user asks to set up, configure, onboard, or troubleshoot brojev, right after installing the plugin, or when run reports a missing or rejected key.
---

# brojev setup

Walk the user through these steps in order. Keep each message short, say what you found, and only ask when a choice is theirs. Skip steps that are already fine.

## 1. Health check

Run `~/.brojev/bin/brojev doctor` with Bash (the plugin links the CLI there on session start; if it is missing, ask the user to restart Claude Code once so the plugin's start hook installs it).

It checks four things:
- **chrome binary**: if missing, the user needs Google Chrome or Chromium (macOS: install Chrome; Linux: `google-chrome-stable` or `chromium`). A non-standard location goes in `BROJEV_CHROME=/path/to/chrome` in `~/.brojev/env`.
- **daemon** and **browser**: `brojev doctor` starts them. A browser timeout with an attached real-Chrome profile usually means Chrome is waiting for the user to click "Allow" on its remote-debugging prompt; tell them to click it, then rerun doctor.
- **jev api key**: see step 2.

On a machine without a display (server, container), add `BROJEV_HEADLESS=1` to `~/.brojev/env`. As root on Linux, brojev adds `--no-sandbox` by itself.

## 2. API key (only `run` needs one)

The primitives (`snapshot`, `click`, `type`, `batch`, ...) work without any key. `run` needs:

- **`openrouter_api_key`** (recommended, one key): pays for Jev through OpenRouter and for the helper model that writes form text. Created at https://openrouter.ai/settings/keys.
- **`typesafe_api_key`** (optional): calls Jev at TypeSafe directly, about 200 ms faster per step. Created at https://console.typesafe.ai/keys.
- **`jev_provider`**: `auto` (default: TypeSafe when its key is set, otherwise OpenRouter), `openrouter`, or `typesafe`.

Tell the user to set them with `/plugin configure brojev@brojev` (stored in the OS keychain), then restart Claude Code so the brojev server picks them up. You cannot run that command for them.

Never ask the user to paste a key into the chat. If they prefer a file (or use the CLI outside Claude Code), they can create it themselves:

```sh
mkdir -p ~/.brojev && printf 'OPENROUTER_API_KEY=...\n' > ~/.brojev/env && chmod 600 ~/.brojev/env
```

`~/.brojev/env` is read on the next `run` without a restart; plugin settings win when both are set. If the user already pasted a key, do not repeat it back; suggest rotating it if the conversation is shared.

## 3. Chrome profile

Call the `profile` tool with no arguments to show the current setup, then explain the choice:

- **`default`** (recommended to start): a dedicated Chrome profile in `~/.brojev/profile`. Nothing personal in it; the user logs in to the sites brojev needs once, in the brojev Chrome window, and the logins persist.
- **The user's real Chrome** (attach mode): brojev uses their existing logins and tabs. Only with their explicit consent: page content from that profile goes into this conversation and, with `run`, to the Jev API. It needs a one-time switch in that Chrome (chrome://inspect/#remote-debugging, turn on remote debugging) and an "Allow" click each time the brojev daemon starts. Steps: `profile` with `action: "chrome-profiles"` lists the people in their Chrome (for example `Default`, `Profile 3`); ask which one, pin the built-in `chrome` profile to it with `action: "add", name: "chrome", chromeProfile: "<person>"`, then `action: "use", name: "chrome"`.

Switching back later: `profile` with `action: "use", name: "default"`.

## 4. Smoke test

1. `navigate` to `https://example.com` and confirm the snapshot shows the page title.
2. If a key is set: `run` with `url: "https://example.com"` and `goal: "Follow the link on the page"`; expect status `done` within a few seconds. A 401 means the key is wrong or belongs to the other provider (check `jev_provider`).

## 5. Wrap up

Summarize in a few lines: Chrome and daemon status, which key and provider are active (from doctor), which profile is in use, and the smoke test result. Point to the main `brojev` skill for everyday use: `run` for whole sub-tasks, `batch` for several known steps, single primitives for exact control.
