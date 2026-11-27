#!/bin/sh
set -eu

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: sh tool/render-tuna-launch-agent.sh /absolute/path/to/tuna /absolute/path/to/log [port]" >&2
  exit 64
fi

binary=$1
log=$2
port=${3-49783}
case "$binary" in /*) ;; *) echo "binary path must be absolute" >&2; exit 64 ;; esac
case "$log" in /*) ;; *) echo "log path must be absolute" >&2; exit 64 ;; esac
if [ "$binary" = "$log" ]; then
  echo "binary and log paths must differ" >&2
  exit 64
fi
case "$port" in ''|0*|*[!0-9]*) echo "port must be between 1 and 65535" >&2; exit 64 ;; esac
if [ "${#port}" -gt 5 ] || [ "$port" -gt 65535 ]; then
  echo "port must be between 1 and 65535" >&2
  exit 64
fi

plist=$(mktemp "${TMPDIR:-/tmp}/tuna-launch-agent.XXXXXXXX")
trap 'rm -f "$plist"' EXIT
trap 'exit 1' HUP INT TERM
/usr/bin/plutil -create xml1 "$plist"
/usr/bin/plutil -insert Label -string "com.github.boumenot.tuna" "$plist"
/usr/bin/plutil -insert ProgramArguments -array "$plist"
/usr/bin/plutil -insert ProgramArguments.0 -string "$binary" "$plist"
/usr/bin/plutil -insert ProgramArguments.1 -string service "$plist"
/usr/bin/plutil -insert ProgramArguments.2 -string --port "$plist"
/usr/bin/plutil -insert ProgramArguments.3 -string "$port" "$plist"
/usr/bin/plutil -insert RunAtLoad -bool true "$plist"
/usr/bin/plutil -insert KeepAlive -bool true "$plist"
/usr/bin/plutil -insert ThrottleInterval -integer 30 "$plist"
/usr/bin/plutil -insert Umask -integer 63 "$plist"
/usr/bin/plutil -insert StandardErrorPath -string "$log" "$plist"
/usr/bin/plutil -convert xml1 -o - "$plist"
