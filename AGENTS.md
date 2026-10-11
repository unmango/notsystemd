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

- `cmd/notsystemd`: the CLI (`run`, `is-active`, `show`, `notify`), stdlib `flag` only.
- `internal/unit`: unit file parsing (`File`, drop-ins), Exec line parsing and expansion (`Command`), environment files, and `Service`, the typed view of `[Service]`.
- `internal/supervisor`: the lifecycle. `reaper.go` owns every `wait4`; nothing else may wait on a child, since a second waiter would steal statuses.
- `internal/notify`: the `$NOTIFY_SOCKET` listener and a sender.
- `internal/state`: the per-unit JSON state file other containers and probes read.

## Conventions

- Behaviour follows systemd's documented semantics (`systemd.service(5)`, `systemd.exec(5)`, `systemd.kill(5)`) unless there is a container reason not to; when departing from them, say why in a comment and in `README.md`.
- No runtime dependencies beyond the Go standard library, and the binary is built with `CGO_ENABLED=0`, so an image can copy it in without a libc.
- Tests are Ginkgo/Gomega. The supervisor specs run real processes: `/bin/sh` and unqualified coreutils resolved through `PATH`, because the Nix build sandbox has `/bin/sh` and nothing else under `/bin`.
  The suite's `TestMain` turns the test binary into a notify daemon when `NOTSYSTEMD_HELPER` is set; add a mode there rather than shipping a fixture binary.
