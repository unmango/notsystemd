# AGENTS.md

This file provides guidance to AI agents when working with code in this repository.

## What this is

A Go command that runs one systemd service unit inside a container without systemd, as the container's entrypoint.
It exists so that vendor software built for a systemd-booted image (UniFi OS Server, at first) can be split into one container per service, each running the vendor's own unit file.
`README.md` lists what is supported; keep it in step with the code.

## Commands

```sh
make test   # go tool ginkgo run -r
make build  # nix build .#; the package's check phase runs go test ./...
make check  # nix flake check (treefmt: gofmt, nixfmt, mdformat, actionlint)
make fmt    # nix fmt
make tidy   # go mod tidy + regenerate nix/gomod2nix.toml; run after any go.mod change
```

## Layout

- `cmd/notsystemd`: the CLI (`run`, `is-active`, `show`, `notify`, and the shims), stdlib `flag` only. It dispatches to a shim when `argv[0]` is one of `shim.Names`.
- `internal/unit`: unit file parsing (`File`, drop-ins), Exec line parsing and expansion (`Command`), environment files, and `Service`, the typed view of `[Service]`.
- `internal/supervisor`: the lifecycle. `reaper.go` owns every `wait4`; nothing else may wait on a child, since a second waiter would steal statuses.
- `internal/notify`: the `$NOTIFY_SOCKET` listener and a sender.
- `internal/dbus`: the D-Bus wire protocol and a server that is both the system bus and `org.freedesktop.systemd1`, answering from the state files. Little-endian only, no file descriptors.
- `internal/state`: the per-unit JSON state file other containers and probes read.
- `internal/shim`: `sudo`, `systemctl`, `systemd-run` and `timedatectl` stand-ins, answered from the state files. `Exec` replaces the process, so its specs run it in a subprocess through the suite's `TestMain`.

## Conventions

- Behaviour follows systemd's documented semantics (`systemd.service(5)`, `systemd.exec(5)`, `systemd.kill(5)`) unless there is a container reason not to; when departing from them, say why in a comment and in `README.md`.
- No runtime dependencies beyond the Go standard library, and the binary is built with `CGO_ENABLED=0`, so an image can copy it in without a libc.
- Tests are Ginkgo/Gomega. The supervisor specs run real processes: `/bin/sh` and unqualified coreutils resolved through `PATH`, because the Nix build sandbox has `/bin/sh` and nothing else under `/bin`.
  The suite's `TestMain` turns the test binary into a notify daemon when `NOTSYSTEMD_HELPER` is set; add a mode there rather than shipping a fixture binary.

## Agent skills

### Issue tracker

Issues are tracked in this repo's GitHub Issues, through the `gh` CLI.
See `docs/agents/issue-tracker.md`.

### Triage labels

The five default triage labels, each named after its role: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`.
See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: one `CONTEXT.md` and `docs/adr/` at the repo root.
See `docs/agents/domain.md`.
