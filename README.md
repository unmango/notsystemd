# notsystemd

A pod-native stand-in for systemd, for software that expects to run under it.

Some vendor software ships as a systemd-booted image: a Debian userland with `/sbin/init` as PID 1 and a dozen units that notify readiness, ping a watchdog, and expect pre-start hooks to have run.
Splitting that into one container per service means each container needs whatever systemd did for its unit.
notsystemd is that: run as a container's entrypoint, it reads the vendor's own `.service` file and runs it the way systemd would, so the image definition is the unit file and nothing more.

```console
$ notsystemd run /lib/systemd/system/unifi-core.service
```

## Commands

- `notsystemd run [--drop-in-dir DIR]... UNIT_FILE` runs a service unit until it exits or notsystemd gets SIGTERM or SIGINT.
  SIGHUP runs `ExecReload=`.
  It exits with the service's status.
- `notsystemd is-active UNIT` exits 0 while the unit is active, for a readiness probe.
- `notsystemd show UNIT` prints the unit's recorded state as JSON.
- `notsystemd notify [--ready] [--status TEXT] [KEY=VALUE]...` sends a notification to `$NOTIFY_SOCKET`, like `systemd-notify`.
- `notsystemd sudo`, `systemctl`, `systemd-run` and `timedatectl` stand in for those commands; see [Shims](#shims).

Every command takes `--state-dir` (default `/run/notsystemd`).
`run` writes `<state-dir>/<unit>.json` on every state change, with the unit's `ActiveState`, `SubState`, `StatusText`, `MainPID`, result and its own PID, and puts the notify socket beside it.
Mount the directory as an `emptyDir` shared by the pod's containers and each can read the others' state.

## What `run` supports

- `Type=simple`, `exec`, `notify`, `forking` (with or without `PIDFile=`) and `oneshot` (with `RemainAfterExit=`).
  `notify-reload` runs as `notify`; `dbus` and `idle` run as `simple`.
- `ExecStartPre=`, `ExecStart=`, `ExecStartPost=`, `ExecReload=`, `ExecStop=` and `ExecStopPost=`, with the `-`, `+`, `!`, `:` and `@` prefixes, quoting, `;`-separated command lists, and `$VAR`/`${VAR}` expansion.
  `ExecStop=` and `ExecStopPost=` see `MAINPID`, `SERVICE_RESULT`, `EXIT_CODE` and `EXIT_STATUS`.
- `User=`, `Group=`, `WorkingDirectory=` (including `-` and `~`), `UMask=` and `PermissionsStartOnly=`.
- `Environment=` and `EnvironmentFile=` (including `-`), layered over the container's own environment.
- `RuntimeDirectory=`, `StateDirectory=`, `CacheDirectory=`, `LogsDirectory=`, `ConfigurationDirectory=` and their `*Mode=`, created and owned by the unit's user; the runtime directory is removed on stop.
- `$NOTIFY_SOCKET` with `READY=1`, `STATUS=`, `MAINPID=`, `RELOADING=1`, `STOPPING=1`, `WATCHDOG=1`, `WATCHDOG=trigger` and `WATCHDOG_USEC=`, accepted from any process (`NotifyAccess=all`).
- `WatchdogSec=` and `WatchdogSignal=`: a missed ping signals the main process and fails the unit.
- `TimeoutStartSec=`, `TimeoutStopSec=`, `TimeoutSec=`, `KillMode=` and `KillSignal=`, escalating to `SIGKILL` when a stop times out.
- `SuccessExitStatus=`, with exit statuses and signal names.
- `LimitNOFILE=`, set on notsystemd itself so every command the unit runs inherits it, and lowered to what the kernel allows as systemd does.
- Drop-ins from `<unit>.d/*.conf` and from `--drop-in-dir`, and the `%n`, `%N`, `%p`, `%i`, `%t`, `%S`, `%C`, `%L` and `%E` specifiers.

As PID 1 it reaps every orphan in the container, and treats the whole container as the unit's control group when stopping it.
Anywhere else it makes itself a child subreaper, so a daemon that double-forks stays visible.

`Restart=` is not implemented on purpose: the container exits with the service and Kubernetes restarts it.
Dependencies between units (`After=`, `Requires=`, `Wants=`) are the pod's business too.

## Shims

The software notsystemd exists for does more than get started by systemd: it runs `systemctl`, `timedatectl` and `systemd-run`, through `sudo`.
Invoked under one of those names, notsystemd answers as that command, so an image links them to it; the flake's `shims` package holds the links.
The shims find unit state in `$NOTSYSTEMD_STATE_DIR`, default `/run/notsystemd`.

- `systemctl` answers `is-active`, `is-failed`, `is-enabled`, `show` (with `-p`/`--property` and `--value`), `status` and `is-system-running` from the state directory.
  A unit the pod does not run reads as inactive, not enabled, and `LoadState=not-found`.
  `stop`, `restart` and their variants send the unit's notsystemd `SIGTERM`, so its container exits and Kubernetes starts it again; `reload` sends it `SIGHUP`, and `kill` signals the main process.
  Signalling a unit in another container needs `shareProcessNamespace: true` on the pod.
  `start`, `enable`, `disable`, `mask`, `unmask`, `daemon-reload` and `reset-failed` succeed without doing anything, since the pod spec decides which units run.
- `timedatectl` reports NTP as enabled and synchronised, because a container's clock is the node's, and accepts `set-*` without changing anything.
- `systemd-run` runs its command in place, as `--uid`/`--gid` with `--setenv`, waiting for it under `--wait` or `--pipe` and starting it in the background otherwise.
- `sudo` runs its command as `-u`/`-g`, keeping the environment apart from `HOME`, `USER` and `LOGNAME`, and asks for no password.
  Only root can switch users, as with sudo's `-n`.

## Planned

A D-Bus facade serving `org.freedesktop.systemd1` from the state directory, for software that watches units' `ActiveState` and `StatusText` over the system bus.

## Development

`nix develop` (or direnv) provides Go, ginkgo and gomod2nix.

```sh
make test   # go tool ginkgo run -r
make build  # nix build .#, which also runs the specs
make check  # nix flake check
make tidy   # go mod tidy, then regenerate nix/gomod2nix.toml
```
