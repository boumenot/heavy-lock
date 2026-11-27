# v2 broker supervision on macOS

The v2 prototype can run as one per-user launchd agent, independently of
terminals and the monitor. The renderer below creates a launchd property list
with `RunAtLoad`, `KeepAlive`, a 30-second restart throttle and a private
process umask. launchd retries a failed startup, including a port conflict or
bad storage; it never chooses another port. Inspect the error log and stop the
agent before correcting the cause.

The target is Apple Silicon macOS 26+. Development and isolated automated
checks have been run on macOS 26.7.3/arm64 with Xcode 27.2 and Go 1.27.3.
An isolated manual check covered installation, login startup, crash restart,
port conflict and retry, running-job recovery, same-build upgrade and
uninstall. A cross-version upgrade or rollback and real workloads have not
been verified.

The singleton lock is `broker.lock` under the OS account's home directory at
`Library/Application Support/heavy-lock/v2`, regardless of `$HOME`, `--port`
or `--data-dir`. Never remove or replace this file: doing so can split the
gate. The database and its `-wal` and `-shm` files must reside together on a
local filesystem supporting SQLite WAL and macOS `flock`; network filesystems
are unsupported. The broker creates new state directories with mode 0700 and
files with mode 0600, and refuses pre-existing state directories or storage
files with group/other mode permissions, symlinks, access control lists, and
files owned by another user. All ACLs are rejected conservatively, including
those that would not grant access.
It does not replace corrupt or incompatible storage. Check and repair any
reported permissions or corruption deliberately; do not delete the database,
WAL, shared-memory file or lock to make an error disappear.

Earlier v2 prototypes used `--data-dir` to choose their lock location. Stop
every older v2 broker before using this version; mixing versions with different
data directories can bypass the singleton.

The service is unauthenticated: private files do not prevent other local
processes or browser origins from using its loopback APIs. The v1 and v2
gates are independent. Do not cut over actual heavy workloads or run them
through both gates without a separately coordinated migration.

## Install

These are instructions, not an action performed by the build. Installing a
service or changing an existing one requires separate permission. Run in a
logged-in GUI session as the intended user, from this checkout, after building
`build/tuna/tuna` under the shared v1 heavy-work lock:

```sh
/Users/boumenot/.local/bin/heavy-lock -- nix develop -c dart tool/dev.dart build tuna
```

The following installs a new agent only; it refuses existing named artifacts.
Choose another port if desired and use that same port explicitly for clients.
Paths containing spaces are supported. `$HOME` must be the OS account's normal
home for these installation paths.

```sh
set -eu
umask 077
label=com.github.boumenot.heavy-lock.tuna
bin="$HOME/Library/Application Support/heavy-lock/bin/tuna"
state="$HOME/Library/Application Support/heavy-lock/v2"
plist="$HOME/Library/LaunchAgents/$label.plist"
log="$state/service.log"
port=49783
if [ -e "$bin" ] || [ -L "$bin" ] || [ -e "$plist" ] || [ -L "$plist" ]; then
  echo "An installation artifact already exists; inspect it first." >&2
  exit 1
fi
launchctl print "gui/$(id -u)" >/dev/null
if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  echo "Agent already loaded; inspect it before proceeding." >&2
  exit 1
fi
mkdir -p -m 700 "$(dirname "$bin")" "$state" "$(dirname "$plist")"
install -m 700 build/tuna/tuna "$bin"
: >> "$log"
sh tool/render-tuna-launch-agent.sh "$bin" "$log" "$port" > "$plist"
# Optional example: uncomment these four lines to set capacity 4 and strict FIFO.
# plutil -insert ProgramArguments.4 -string --capacity "$plist"
# plutil -insert ProgramArguments.5 -string 4 "$plist"
# plutil -insert ProgramArguments.6 -string --backfill-limit "$plist"
# plutil -insert ProgramArguments.7 -string 0 "$plist"
plutil -lint "$plist"
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl print "gui/$(id -u)/$label"
tail -n 50 "$log"
```

The log must show `ready on 127.0.0.1:PORT`; `launchctl bootstrap` by itself
does not prove readiness. If startup fails, inspect the log, the selected port
and filesystem permissions. No service is started on demand by clients; use
`tuna --port PORT -- command ...` only after a separately authorized cutover.
Do not treat a GUI login agent as proof of availability in an SSH-only session.

Capacity defaults to the automatic v1-compatible value; `--capacity` accepts a
positive 32-bit integer. Backfill defaults to 1; `--backfill-limit` accepts a
non-negative 32-bit integer and 0 selects strict FIFO. These settings are fixed
for one broker run. Change an existing agent only with separate authorization
and after draining jobs; retain reservations and state across restart. A lower
capacity does not release existing reservations, and oversized queued jobs
remain queued with a warning. V1 `HEAVY_LOCK_CAPACITY` does not configure v2.

## Upgrade and uninstall

Do not upgrade while v2 jobs are queued or running. The isolated crash-recovery
check below does not establish that a live upgrade is safe. Preserve the
database and all sidecars. After a separately authorized upgrade and a new
gated build, stop the job with
`launchctl bootout "gui/$(id -u)" "$plist"`, save the previous executable,
replace it atomically, and bootstrap the same plist again. For example, with
the variables above still set:

```sh
if [ -e "$bin.previous" ] || [ -L "$bin.previous" ] || [ -e "$bin.new" ] || [ -L "$bin.new" ]; then
  echo "Upgrade artifact already exists; inspect it first." >&2
  exit 1
fi
cp -p "$bin" "$bin.previous"
install -m 700 build/tuna/tuna "$bin.new"
mv -f "$bin.new" "$bin"
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl print "gui/$(id -u)/$label"
tail -n 50 "$log"
```

If the upgrade fails, boot out the agent before restoring the saved executable
and investigate the log. Database schema changes may make rollback unsafe;
do not assume an older executable can read a newer database. Back up state
only with the broker stopped and retain the database and sidecars together.

To uninstall after separate authorization and after draining jobs:

```sh
launchctl bootout "gui/$(id -u)" "$plist"
rm "$plist" "$bin"
```

This leaves state, logs and the singleton lock intact. Do not unlink the lock.
The optional `$bin.previous` backup is also retained.

## Manual launchd check

With separately authorized changes, use an isolated test macOS user/GUI
session with no existing tuna service or real workloads. Install the agent
above, log out and back in, and confirm launchd reports the job and the log
shows readiness on the selected port. In that session, set the variables from
the installation commands again and inspect
`launchctl print "gui/$(id -u)/$label"` to identify the isolated broker PID. With explicit
permission to terminate that test process, run `kill -ABRT <isolated-broker-pid>`
with that exact PID; confirm a different PID and a new readiness line appear.

Run `launchctl bootout "gui/$(id -u)" "$plist"`. In a separate terminal in
this checkout start the test listener with
`nix develop -c python3 -m http.server "$port" --bind 127.0.0.1`; leave it
running. Bootstrap the agent with `launchctl bootstrap "gui/$(id -u)" "$plist"`
and inspect `tail -n 50 "$log"`: it must report the port conflict, with no
readiness on another port. Stop the test listener with Ctrl+C, wait at least
30 seconds for the retry, and confirm the original port becomes ready. Finally
uninstall the isolated agent. Record the actual host, commands, results and
any failures.

On November 26, 2026, the user ran a separately authorized check on
macOS 26.7.3/arm64 in the `tunatest` account's logged-in GUI domain. Commands
ran through `sudo /bin/launchctl asuser 502 /usr/bin/sudo -iu tunatest` on
port 49790. Installation, logout/login startup and crash restart passed. An
`nc -lk 127.0.0.1 49790` listener produced the expected port-conflict error;
launchd retried successfully after the listener was released.

A running `/bin/sleep 120` survived a broker crash with the same process ID
and attempt number, then finished with exit code 0. A same-build upgrade and
uninstall passed; the check did not exercise a version change or rollback.
The test removed its agent and upgrade backup but retained its database,
singleton lock, logs and results. Detailed results are recorded in
[#17](https://github.com/boumenot/heavy-lock/issues/17).
