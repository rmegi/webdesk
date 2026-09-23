#!/bin/sh
# Stands in for someone logged in at the machine's monitor: an XFCE session
# on a virtual X11 screen.
set -eu

export DISPLAY=:0
Xvfb "$DISPLAY" -screen 0 "${SCREEN_SIZE:-1600x900}x24" -nolisten tcp &

i=0
until xdpyinfo >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    echo "Xvfb did not start" >&2
    exit 1
  fi
  sleep 0.1
done

exec dbus-launch --exit-with-session startxfce4
