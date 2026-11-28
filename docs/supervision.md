# Broker supervision on macOS

The Tuna broker runs as one per-user launchd agent, independently of
terminals and the monitor. The renderer below creates a launchd property list
with `RunAtLoad`, `KeepAlive`, a 30-second restart throttle and a private
process umask. launchd retries a failed startup, including a port conflict or
bad storage; it never chooses another port. Inspect the error log and stop the
agent before correcting the cause.

The target is Apple Silicon macOS 26+. Development and isolated automated
checks have been run on macOS 26.7.3/arm64 with Xcode 27.2 and Go 1.27.3.
An isolated manual check covered installation, login startup, crash restart,
port conflict and retry, running-job recovery, same-build upgrade and
uninstall. A cross-version upgrade or rollback has not been verified.

The singleton lock is `broker.lock` under the OS account's home directory at
`Library/Application Support/tuna`, regardless of `$HOME`, `--port`
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

Earlier builds used other lock locations (`--data-dir`, then
`Library/Application Support/heavy-lock/v2`). Stop every older broker before
using this version; brokers with different lock locations do not share a
singleton or a queue. See [migrating an earlier installation](#migrating-an-earlier-installation).

The service is unauthenticated: private files do not prevent other local
processes or browser origins from using its loopback APIs.

## Install

These are instructions, not an action performed by the build. Installing a
service or changing an existing one requires separate permission. Run in a
logged-in GUI session as the intended user, from this checkout. First build
and test all supported apps through the shared broker, then separately
authorize deployment, stop/drain any old broker and quit the monitor:

```sh
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart check all
nix develop -c dart tool/dev.dart install all
```

These install the validated runner at `~/.local/bin/tuna` and monitor at
`~/Applications/Tuna Monitor.app`; they do not install/load an agent. See the
[artifact and replacement safeguards](../README.md#install-and-supervision).
On a machine without a compatible running broker, first bootstrap/cutover
requires explicit coordinated authorization. Do not run heavy work unlocked.

The following installs a new agent only; it requires the installed runner
and refuses an existing agent configuration.
Choose another port if desired and use that same port explicitly for clients.
Paths containing spaces are supported. `$HOME` must be the OS account's normal
home for these installation paths.

```sh
set -eu
umask 077
label=com.github.boumenot.tuna
bin="$HOME/.local/bin/tuna"
state="$HOME/Library/Application Support/tuna"
plist="$HOME/Library/LaunchAgents/$label.plist"
log="$state/service.log"
port=49783
if [ ! -x "$bin" ] || [ -L "$bin" ]; then
  echo "Run install all first; the runner must be a regular executable." >&2
  exit 1
fi
if [ -e "$plist" ] || [ -L "$plist" ]; then
  echo "An agent configuration already exists; inspect it first." >&2
  exit 1
fi
launchctl print "gui/$(id -u)" >/dev/null
if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
  echo "Agent already loaded; inspect it before proceeding." >&2
  exit 1
fi
mkdir -p -m 700 "$state" "$(dirname "$plist")"
: >> "$log"
sh tool/render-tuna-launch-agent.sh "$bin" "$log" "$port" > "$plist"
# Optional example: uncomment these four lines to set capacity 4 and disable backfill.
# plutil -insert ProgramArguments.4 -string --capacity "$plist"
# plutil -insert ProgramArguments.5 -string 4 "$plist"
# plutil -insert ProgramArguments.6 -string --backfill-limit "$plist"
# plutil -insert ProgramArguments.7 -string 0 "$plist"
plutil -lint "$plist"
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl print "gui/$(id -u)/$label"
tail -n 50 "$log"
```

Clients, the `tuna` skill and this agent use the same regular executable
at `/Users/boumenot/.local/bin/tuna`; no extra symlink is needed.

The log must show `ready on 127.0.0.1:PORT`; `launchctl bootstrap` by itself
does not prove readiness. If startup fails, inspect the log, the selected port
and filesystem permissions. No service is started on demand by clients; use
`tuna --port PORT -- command ...` once the broker is ready.
Do not treat a GUI login agent as proof of availability in an SSH-only session.

Capacity defaults to an automatic value from the machine's processors and
memory (clamped to 2-8); `--capacity` accepts a
positive 32-bit integer. Backfill defaults to 1; `--backfill-limit` accepts a
non-negative 32-bit integer and 0 disables backfill. These settings are fixed
for one broker run. Change an existing agent only with separate authorization
and after draining jobs; retain reservations and state across restart. A lower
capacity does not release existing reservations, and oversized queued jobs
remain queued with a warning.

## Upgrade and uninstall

Do not upgrade while jobs are queued or running. The isolated crash-recovery
check below does not establish that a live upgrade is safe. Preserve the
database and all sidecars. After a separately authorized upgrade and a new
gated build, stop the job with
`launchctl bootout "gui/$(id -u)" "$plist"`, save the previous executable,
replace it atomically, and bootstrap the same plist again. `install all`
refuses a running broker or monitor; it does not perform the bootout/bootstrap
steps. For example, with
the variables above still set:

```sh
set -eu
if [ -e "$bin.previous" ] || [ -L "$bin.previous" ]; then
  echo "Upgrade artifact already exists; inspect it first." >&2
  exit 1
fi
cp -p "$bin" "$bin.previous"
nix develop -c dart tool/dev.dart install all
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl print "gui/$(id -u)/$label"
tail -n 50 "$log"
```

Before upgrading, also preserve the matching old monitor bundle if a rollback
after service startup may be needed. If the artifact replacement fails, the
installer restores both old apps; do not bootstrap after a failed install.
If new service startup fails, boot out the agent before restoring the saved
executable and matching monitor, and investigate the log. Database schema
changes may make rollback unsafe;
do not assume an older executable can read a newer database. Back up state
only with the broker stopped and retain the database and sidecars together.

To uninstall after separate authorization and after draining jobs:

```sh
launchctl bootout "gui/$(id -u)" "$plist"
rm "$plist" "$bin"
```

This leaves state, logs and the singleton lock intact. Do not unlink the lock.
The optional `$bin.previous` backup is also retained.

## Migrating an earlier installation

Earlier builds installed the agent `com.github.boumenot.heavy-lock.tuna`
running `~/Library/Application Support/heavy-lock/bin/tuna`, with state in
`~/Library/Application Support/heavy-lock/v2`. The current build changes the
contract identity (`tuna`, service version `3.0.0-dev.2`), the REST paths
(no `/v1` prefix), the default state and lock directory and the agent label,
so old and new runners, brokers and monitors refuse each other and must be
replaced together. History is not migrated; the new broker starts with an
empty database. With separate authorization, once no jobs are queued or
running:

1. `launchctl bootout "gui/$(id -u)/com.github.boumenot.heavy-lock.tuna"` and
   move its plist out of `~/Library/LaunchAgents`.
2. Quit any running Tuna Monitor.
3. Obtain explicit authorization for the bootstrap build/test while no
   compatible shared broker exists; the normal heavy gate and any freeze
   cannot be bypassed implicitly. Produce the new validated app set with
   `check all`.
4. Inspect and deliberately remove the old `~/.local/bin/tuna` symlink
   (never its target or broker state) with separate permission, run
   `install all`, and create the new agent with the [install](#install)
   commands above. An agent already pointing at an older executable needs
   separately authorized reconfiguration to `~/.local/bin/tuna`.
5. Verify new broker readiness, then relaunch the newly installed matching
   monitor.

The old state directory, including its lock and database, is left untouched;
archive or remove it only deliberately, after the old broker has stopped.

## Manual launchd check

With separately authorized changes, use an isolated test macOS user/GUI
session with no existing Tuna service or real workloads. Install the agent
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
