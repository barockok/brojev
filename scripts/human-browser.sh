#!/bin/sh
# A visible Chrome a person can reach from their own browser, for brojev's
# handoff tool: Xvfb display -> Chrome (DevTools on 127.0.0.1) -> x11vnc ->
# noVNC (websockify). brojev copies the agent's cookies into this Chrome, the
# person finishes the step (login, CAPTCHA, 2FA), brojev copies them back.
#
#   scripts/human-browser.sh start | stop | status
#
# Needs: Xvfb, x11vnc, websockify, noVNC (Debian/Ubuntu: apt install xvfb
# x11vnc novnc websockify) and Chrome or Chromium.
#
# Every port binds to 127.0.0.1. Reach noVNC through an SSH tunnel
# (ssh -L 6080:127.0.0.1:6080 host) or a private network; set
# BROJEV_HUMAN_BIND=0.0.0.0 only behind one, and always with a password.
#
# Environment (all optional):
#   BROJEV_HOME            state dir (default ~/.brojev); profile, logs, pids
#   BROJEV_CHROME          Chrome binary (default: first found in PATH)
#   BROJEV_HUMAN_DISPLAY   X display number (default 99)
#   BROJEV_HUMAN_SIZE      screen size (default 1280x800)
#   BROJEV_HUMAN_CDP_PORT  Chrome DevTools port (default 9223)
#   BROJEV_HUMAN_VNC_PORT  VNC port (default 5901)
#   BROJEV_HUMAN_WEB_PORT  noVNC port (default 6080)
#   BROJEV_HUMAN_BIND      noVNC bind address (default 127.0.0.1)
#   BROJEV_HUMAN_PASSWORD  VNC password (default: generated, saved in human/vnc-password)
#   BROJEV_HUMAN_HOST      host name put in the printed view URL (default localhost)
#   NOVNC_DIR              noVNC web root (default /usr/share/novnc)
set -e

home="${BROJEV_HOME:-$HOME/.brojev}"
dir="$home/human"
disp="${BROJEV_HUMAN_DISPLAY:-99}"
size="${BROJEV_HUMAN_SIZE:-1280x800}"
cdp="${BROJEV_HUMAN_CDP_PORT:-9223}"
vnc="${BROJEV_HUMAN_VNC_PORT:-5901}"
web="${BROJEV_HUMAN_WEB_PORT:-6080}"
bind="${BROJEV_HUMAN_BIND:-127.0.0.1}"
host="${BROJEV_HUMAN_HOST:-localhost}"
novnc="${NOVNC_DIR:-/usr/share/novnc}"

alive() { [ -f "$dir/$1.pid" ] && kill -0 "$(cat "$dir/$1.pid")" 2>/dev/null; }

spawn() { # name, command...
  name="$1"; shift
  if alive "$name"; then return; fi
  nohup "$@" >"$dir/$name.log" 2>&1 &
  echo $! >"$dir/$name.pid"
}

find_chrome() {
  if [ -n "$BROJEV_CHROME" ]; then echo "$BROJEV_CHROME"; return; fi
  for c in google-chrome google-chrome-stable chromium chromium-browser; do
    if command -v "$c" >/dev/null 2>&1; then command -v "$c"; return; fi
  done
  echo "human-browser: Chrome not found; set BROJEV_CHROME" >&2; exit 1
}

wait_for() { # what, test command...
  what="$1"; shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ $i -gt 100 ]; then echo "human-browser: $what did not start; see $dir" >&2; exit 1; fi
    sleep 0.1
  done
}

view_url() { echo "http://$host:$web/vnc.html?autoconnect=1&resize=scale"; }

case "${1:-status}" in
start)
  for b in Xvfb x11vnc websockify; do
    command -v "$b" >/dev/null 2>&1 || { echo "human-browser: $b not installed" >&2; exit 1; }
  done
  [ -f "$novnc/vnc.html" ] || { echo "human-browser: noVNC not found in $novnc (set NOVNC_DIR)" >&2; exit 1; }
  chrome="$(find_chrome)"
  mkdir -p "$dir" && chmod 700 "$dir"
  if [ -n "$BROJEV_HUMAN_PASSWORD" ]; then
    pw="$BROJEV_HUMAN_PASSWORD"
  elif [ -s "$dir/vnc-password" ]; then
    pw="$(cat "$dir/vnc-password")"
  else
    pw="$(head -c 12 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-8)"
    (umask 077; printf '%s' "$pw" >"$dir/vnc-password")
  fi
  (umask 077; x11vnc -storepasswd "$pw" "$dir/vnc.pass" >/dev/null 2>&1)

  spawn xvfb Xvfb ":$disp" -screen 0 "${size}x24" -nolisten tcp
  wait_for "Xvfb" test -e "/tmp/.X11-unix/X$disp"

  nosandbox=""
  [ "$(id -u)" = 0 ] && nosandbox="--no-sandbox" # Chrome refuses to run sandboxed as root
  w="${size%x*}"; h="${size#*x}"
  export DISPLAY=":$disp"
  # shellcheck disable=SC2086
  spawn chrome "$chrome" $nosandbox --user-data-dir="$dir/profile" \
    --remote-debugging-port="$cdp" --remote-debugging-address=127.0.0.1 \
    --no-first-run --no-default-browser-check --window-position=0,0 --window-size="$w,$h" \
    --start-maximized about:blank
  wait_for "Chrome DevTools" curl -sf "http://127.0.0.1:$cdp/json/version"

  spawn x11vnc x11vnc -display ":$disp" -rfbport "$vnc" -localhost -rfbauth "$dir/vnc.pass" -forever -shared -noxdamage
  spawn novnc websockify --web "$novnc" "$bind:$web" "127.0.0.1:$vnc"
  wait_for "noVNC" curl -sf "http://127.0.0.1:$web/vnc.html"

  echo "human browser ready"
  echo "  view:      $(view_url)"
  echo "  password:  $pw   (also in $dir/vnc-password)"
  echo "  for brojev (in the environment or $home/env, then: brojev stop):"
  echo "    BROJEV_HUMAN_CDP_URL=http://127.0.0.1:$cdp"
  echo "    BROJEV_HUMAN_VIEW_URL=$(view_url)"
  ;;
stop)
  for n in novnc x11vnc chrome xvfb; do
    if alive "$n"; then kill "$(cat "$dir/$n.pid")" 2>/dev/null || true; fi
    rm -f "$dir/$n.pid"
  done
  echo "human browser stopped"
  ;;
status)
  for n in xvfb chrome x11vnc novnc; do
    if alive "$n"; then echo "  $n: running (pid $(cat "$dir/$n.pid"))"; else echo "  $n: stopped"; fi
  done
  alive novnc && echo "  view: $(view_url)"
  ;;
*)
  echo "usage: $0 start | stop | status" >&2; exit 2
  ;;
esac
