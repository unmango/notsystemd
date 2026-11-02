# Keeping an optional heavy Go dependency out of the core binary

Research for [unmango/notsystemd#13](https://github.com/unmango/notsystemd/issues/13): the **Kubernetes backend** may use `k8s.io/client-go`, but `notsystemd` itself must stay standard-library-only and `CGO_ENABLED=0`, and it is also exec'd once per call as the `systemctl`, `sudo`, `systemd-run` and `timedatectl` **Shims**.
What are the established ways to keep such a dependency out of the core binary, and what does each cost here?
Facts and trade-offs only; no decision.

Gathered 2026-11-02.
Measurements were taken in a scratch directory outside the repo, on a 4-core Intel Xeon container (linux/amd64), Go 1.25.0 for notsystemd and the Go 1.26.10 toolchain that client-go requires (see 1.2), all binaries built with `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`.
Primary sources: the Go Modules Reference ([`_content/ref/mod.md` in golang/website](https://github.com/golang/website/blob/master/_content/ref/mod.md), rendered at go.dev/ref/mod), `go help` output, the gomod2nix source at the revision this flake locks (`1201ddd1`), and the kubectl, client-go, go-plugin and CNI sources/specs cited inline.

## 1. What client-go costs

### 1.1 Binary size, modules, start-up

The test program is a `main` that calls `rest.InClusterConfig()`, `kubernetes.NewForConfigOrDie()` and `CoreV1().Pods("").List()`, built against `k8s.io/client-go v0.37.2` (the latest on proxy.golang.org on the gathering date).

| binary | size (stripped) | gzip | linked third-party modules | packages linked |
|---|---|---|---|---|
| `notsystemd` today (`./cmd/notsystemd`) | 3.48 MB | 1.49 MB | 0 | 8 non-std (all its own) |
| stdlib-only pod lister (`net/http` + `crypto/tls` + `encoding/json`, hand-rolled) | 5.85 MB | — | 0 | — |
| client-go pod lister | 26.5 MB | 8.73 MB | 46 (`go version -m`) | 577 total, ~380 non-std (`go list -deps`) |
| empty `main` + a `//go:build k8s` file importing `client-go/kubernetes`, built without the tag | 1.60 MB | — | 0 | — |
| same, built with `-tags k8s` | 23.5 MB | — | — | — |

For the client-go program: `go mod tidy` writes 11 direct + 44 indirect requirements to `go.mod`, 127 lines to `go.sum`, `go list -m all` reports 78 modules, and `gomod2nix generate` writes 46 `[mod]` entries (notsystemd's own `nix/gomod2nix.toml` has 14, all test-only).
The 46 modules' source in the module cache is about 26 MB unpacked (`du`), which is what gomod2nix's vendor environment fetches.

Start-up, measured as 1000 sequential runs from a bash loop (so each figure includes bash's fork; three repetitions each), with `main` returning before any network I/O:

| process | per run |
|---|---|
| stdlib-only binary, returns immediately | 1.5–1.6 ms |
| `notsystemd is-active <missing unit>` | 1.5 ms |
| client-go binary, returns immediately | 5.5–5.6 ms |
| a stdlib Go launcher that `os/exec`s the stdlib binary | 2.75–2.9 ms |
| a stdlib Go launcher that `os/exec`s the client-go binary | 6.8–7.2 ms |

`GODEBUG=inittrace=1` on the client-go binary: 236 package `init`s, 3.8 ms of init clock time and 1.86 MB allocated before `main`; the largest is `k8s.io/client-go/kubernetes/scheme` (1.3 ms, 672 KB, 5662 allocs), which registers every built-in API type.
Peak RSS (`getrusage` of the child) was 18.8 MB for the client-go binary against at most 13.2 MB for the stdlib ones.
None of this includes the API call itself: a short-lived process opens a fresh TLS connection to the API server every invocation, whichever option is chosen; that round trip was not measured (no cluster).

So: client-go adds roughly 23 MB to a static binary, 46 linked modules, and about 4 ms of start-up per process; an extra exec hop between two Go processes costs about 1.2 ms.

### 1.2 It also raises the Go version

`k8s.io/client-go v0.37.2`'s `go.mod` says `go 1.26.0` (v0.35.0 says `go 1.25.0`, v0.34.0 `go 1.24.0`; from proxy.golang.org).
`go mod tidy` in a module that requires it raises that module's `go` line to `1.26.0`, observed in both the single-module and nested-module experiments below.
notsystemd's `go.mod` says `go 1.25.0`.
The nixpkgs this flake follows has `go_1_26` at 1.26.5.

## 2. Build tags in one module

Pattern: put the backend in files guarded by `//go:build k8s` (or a separate `main` package in the same module) and build the full variant with `-tags k8s`.

- **The binary**: verified. Without the tag the untagged binary links none of it (1.60 MB, zero `dep` lines in `go version -m`).
- **`go.mod`/`go.sum`**: the dependency lands in both regardless. The Modules Reference, `go mod tidy`: "`go mod tidy` acts as if all build tags are enabled, so it will consider platform-specific source files and files that require custom build tags, even if those source files wouldn't normally be built. There is one exception: the `ignore` build tag is not enabled." ([ref/mod, `go mod tidy`](https://github.com/golang/website/blob/master/_content/ref/mod.md)).
  Verified: tidy on a `go 1.25.0` module with one `//go:build k8s` file added client-go, wrote a 127-line `go.sum` and rewrote the `go` line to `1.26.0`.
  So the core module's `go` directive follows client-go's, and every client-go bump is a change to the core module's `go.mod`.
- **gomod2nix**: `nix/gomod2nix.toml` is generated from that `go.mod`, so it gains the 46 modules.
  `mkVendorEnv` in `builder/default.nix` maps `fetchGoModule` over every entry of `modulesStruct.mod` and symlinks all of them into `vendor/`, with no notion of build tags, so the core package's build fetches and vendors all of client-go even though it links none of it.
  `buildGoApplication` does take a `tags` argument, so the full variant is a second derivation over the same `src`/`modules` with `tags = [ "k8s" ]`.
- **Tests**: `go test ./...` and `go vet ./...` without the tag skip the tagged files, so the backend's code is untested and unchecked unless CI runs them a second time with the tag.
- **Accidental linkage**: nothing in the toolchain stops an untagged file from importing the backend package; the guarantee is a convention (or a check such as asserting `go version -m bin/notsystemd` has no `dep` lines).

## 3. A nested module in the same repo

Pattern: `kube/go.mod` declares `module github.com/unmango/notsystemd/kube`, requires `github.com/unmango/notsystemd` and points it at the parent with `replace github.com/unmango/notsystemd => ../`; the second binary's `main` lives there.

What was verified in a copy of the repo with such a module added:

- **The core module is untouched.** Its `go.mod`, `go.sum` and `go` line do not change; client-go and `go 1.26.0` appear only in `kube/go.mod`.
- **`./...` stops at the module boundary.** `go list ./...`, `go vet ./...` and `go test ./...` from the root do not include `kube/...`. The go command's walk does this explicitly: "Stop at module boundaries" … `if info, err := os.Stat(filepath.Join(pkgDir, "go.mod")); err == nil … return filepath.SkipDir` ([`src/cmd/go/internal/modload/search.go`](https://github.com/golang/go/blob/master/src/cmd/go/internal/modload/search.go)).
  The module zip of the root also excludes it: "Module zip files do not include the contents of `vendor` directories or any nested modules" (ref/mod, module zip files).
- **The nested module can import the root's `internal/` packages.** The rule is by import path: "Code in or below a directory named "internal" is importable only by code in the directory tree rooted at the parent of "internal"" (`go help gopath`, Internal Directories), and `github.com/unmango/notsystemd/kube` is under `github.com/unmango/notsystemd`. Verified: `kube` imported `internal/state` and built.
- **`make test` does cross the boundary.** `go tool ginkgo run -r` (Ginkgo 2.33.1) found and ran a test in `kube/x`; Ginkgo's suite finder (`ginkgo/internal/test_suite.go`) skips only `vendor` directories, not nested modules, and runs `go test` in each suite directory, which then resolves the nested `go.mod`. So the dev loop needs client-go downloaded, while the Nix check phase (`go test ./...`) does not.
- **Tags**: a nested module's version tags are prefixed with its subdirectory: "If the module subdirectory portion of the module path is not empty, then each tag name must be prefixed with the module subdirectory" (ref/mod, mapping versions to commits), e.g. `kube/v0.1.0`. This only matters if someone `go install`s it by version; the repo currently versions through `VERSION`.
- **Replace and consumers**: a `replace` directive only applies in the main module, so `go install github.com/unmango/notsystemd/kube@version` would ignore `=> ../` and need a real, tagged version of the root module it requires.

### 3.1 go.work

`go work init . ./kube` makes both modules main modules, so tooling (gopls, `go build` from either directory) sees edits across both without the `replace`.
Observed costs:

- The workspace's `go` line is the maximum of its modules': the go command switched to Go 1.26.10 for `go build ./cmd/notsystemd` at the root, i.e. the core is built locally with a different toolchain from the one Nix uses.
- Minimal version selection runs over the union: with the workspace on, the root's test dependencies resolved to `golang.org/x/net v0.57.0`, `x/sys v0.47.0`, `x/text v0.40.0`, against `v0.56.0`, `v0.46.0`, `v0.38.0` with `GOWORK=off`. The core's tests then run against versions its own `go.sum`/`gomod2nix.toml` do not pin. The linked core binary was unaffected (it has no dependencies).
- The Modules Reference: "It is generally inadvisable to commit go.work files into version control systems", because a checked-in one can override a developer's own and can make a module's CI pass with different dependency versions than its users get (ref/mod, `go.work` files).

### 3.2 gomod2nix and the flake with two modules

From the gomod2nix source at the locked revision (`builder/default.nix`, `builder/hooks/go-config-hook.sh`):

- **No go.work support.** The repository has no reference to `go.work` or workspaces in its Go, Nix or docs. Builds export `GOFLAGS=-mod=vendor`, `GOPROXY=off`, `GO_NO_VENDOR_CHECKS=1` and use a vendor tree gomod2nix assembles itself, so the per-module `go.mod`/`gomod2nix.toml` is what counts. A `go.work` inside `src` would put the go command in workspace mode against a vendor tree built for one module; keeping it out of the fileset (or `GOWORK=off`) avoids that.
- **One toml per module.** `buildGoApplication` reads one `modules` file. The second derivation passes `modules = ./kube/gomod2nix.toml` (generated with `gomod2nix generate --dir kube`), and `modRoot = "kube"`: the config hook does `cd "${modRoot:-.}"` before building.
- **The local `replace` needs `pwd`.** `mkVendorEnv` turns each `replace` with a `path` into `ln -s ${pwd + "/${value.path}"} vendor/<module>`, and `goMod` is only parsed when `pwd` is set (`goMod = if pwd != null && pathExists goModPath then parseGoMod … else null`). `nix/package.nix` currently passes `src` and `modules` but not `pwd`, so the kube derivation would need `pwd = ../kube` (or equivalent), and the parent directory it points at is then copied into the store as the replacement module.
- **Go version selection.** With `pwd` set, `selectGo` picks the lowest `go_*` attribute at least the `go.mod`'s `go` line; without it, `buildPackages.go`. A kube derivation with `pwd` would therefore select Go 1.26 independently of the core's.
- **The core's fileset must exclude the nested module.** `nix/package.nix` builds `src` from `../go.mod`, `../go.sum` and every `*.go` file under the repo. It would pick up `kube/*.go` but not `kube/go.mod`, so inside the core build `kube/` would look like a package of the root module importing client-go, and the check phase's `go test ./...` would fail to find it in the vendor tree. It also means every edit under `kube/` changes the core derivation's hash. Excluding `../kube` from the fileset avoids both.
- **Make targets.** `make tidy` runs `go mod tidy` and `gomod2nix generate` for the root only; a second module needs both run in `kube/` as well, and both tomls kept in step.

## 4. An out-of-process backend

Pattern: the core stays stdlib-only and hands **Backend** calls to a separate binary that links client-go.

### 4.1 Exec per call

Established examples:

- **kubectl plugins**: "A plugin is a standalone executable file, whose name begins with `kubectl-`" found on `PATH` ([kubectl plugins task page](https://github.com/kubernetes/website/blob/main/content/en/docs/tasks/extend-kubectl/kubectl-plugins.md)); kubectl replaces itself with it: `DefaultPluginHandler.Execute` ends in `syscall.Exec(executablePath, append([]string{executablePath}, cmdArgs...), environment)` ([`staging/src/k8s.io/kubectl/pkg/cmd/plugin.go`](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/kubectl/pkg/cmd/plugin.go)). There is no protocol beyond argv, environment and the inherited stdio.
- **client-go exec credential plugins**: client-go runs `exec.Command(a.cmd, a.args...)`, passes an `ExecCredential` object in the `KUBERNETES_EXEC_INFO` environment variable and reads one back as JSON on stdout, then caches it (`if a.cachedCreds != nil && !a.credsExpired() { return a.cachedCreds, nil }`) until its `expirationTimestamp` ([`plugin/pkg/client/auth/exec/exec.go`](https://github.com/kubernetes/client-go/blob/master/plugin/pkg/client/auth/exec/exec.go); [authentication docs](https://github.com/kubernetes/website/blob/main/content/en/docs/reference/access-authn-authz/authentication.md)). The cache is what keeps this per-call exec off the hot path; a one-shot shim has no process lifetime to cache across.
- **CNI**: the runtime execs the plugin binary per operation, "passes parameters to the plugin via environment variables and configuration. It supplies configuration via stdin", with `CNI_COMMAND` naming the operation (`ADD`, `DEL`, `CHECK`, `GC`, `VERSION`) and a JSON result on stdout ([CNI `SPEC.md`](https://github.com/containernetworking/cni/blob/main/SPEC.md)).

Costs here, per shim invocation, using 1.1:

- The shim already pays one Go process start (~1.5 ms in the loop above). Exec'ing a client-go backend adds the backend's start-up: about 1.2 ms for the hop plus about 4 ms of client-go init, roughly 5–5.5 ms more per `systemctl` call, before the API round trip. With `syscall.Exec` (the kubectl form) the shim's process is replaced rather than waited on, saving the extra fork but not the backend's start-up.
- A fresh TLS connection to the API server per call (no connection reuse, no informer cache), as for any one-shot process.
- Protocol cost is small: argv/env in, exit status and stdout out (the `is-active`/`show` outputs already exist as text). A JSON request/response, as CNI and exec credentials use, is stdlib `encoding/json` on both sides.
- Packaging: two binaries in the image, and the core needs a rule for finding the backend (a fixed path, `PATH` lookup by name like `kubectl-*`, or an environment variable).

### 4.2 A socket to a long-lived backend

**hashicorp/go-plugin** (v1.8.0): "works by launching subprocesses and communicating over RPC (using standard `net/rpc` or gRPC). A single connection is made between any plugin and the host process. For net/rpc-based plugins, we use a connection multiplexing library [yamux]" (its `README.md`). The plugin prints a handshake line (`fmt.Sprintf("%d|%d|%s|%s|%s|%s", …)` in `server.go`: protocol versions, network type and address, protocol) on stdout; `ClientConfig.Reattach` lets a host connect to an already running plugin instead of starting one.
Measured host-side cost: a host that only constructs a `plugin.Client` restricted to `ProtocolNetRPC` links 14 third-party modules, including `google.golang.org/grpc`, `google.golang.org/protobuf`, `github.com/hashicorp/yamux`, `go-hclog` and `fatih/color`, and is 12.45 MB stripped. Using go-plugin on the host side therefore breaks the stdlib-only rule for the core by itself.
Its model also assumes the host outlives the call: the host starts the plugin, and the plugin lives as long as the host's `Client`. A shim exec'd once per `systemctl` call would start and handshake a fresh plugin each time, so it pays the 4.1 start-up cost plus the handshake, unless it reattaches to a plugin something else started.

**A stdlib socket**: the core already does this shape. `notsystemd dbus` is a long-running sidecar the shims' callers reach over `/run/dbus/system_bus_socket` (README, D-Bus section), and the standard library has `net/rpc` and `net/rpc/jsonrpc` (the protocols go-plugin's net/rpc mode wraps) and `net/http` over a Unix socket. A long-lived Kubernetes backend process (a sidecar, or a mode of an existing long-running process) would:

- pay client-go's start-up and TLS handshake once, and could hold a watch or informer cache so a call needs no API round trip;
- leave the shim a stdlib client: connect to a Unix socket, send a request, read a reply — no fork/exec of a second binary per call;
- add a process to run and supervise, a socket to share between containers (as `/run/dbus` already is), a wire format to version, and a failure mode when the backend is not up yet.

## 5. Summary of trade-offs

| | core `go.mod` gets client-go | core binary links it | Nix | per-shim-call cost | extra moving parts |
|---|---|---|---|---|---|
| Build tags, one module | yes, and its `go` line (1.26) | no (verified) | one toml, core build fetches and vendors all 46 modules; second derivation with `tags` | none if the shim is the core binary | tests/vet need a tagged second pass; isolation by convention |
| Nested module | no | no | second toml, `modRoot`, `pwd` for the `replace`, core fileset must exclude it | none if the shim is the core binary | `make tidy` twice; Ginkgo `-r` crosses into it; go.work optional and not for committing |
| Exec per call | no | no | separate package (either layout above) | ~5–5.5 ms backend start-up + fresh TLS per call | backend discovery, second binary in image |
| go-plugin | no for the plugin, but the host links go-plugin (14 modules incl. gRPC) | go-plugin, yes | as above | plugin start + handshake per call unless reattaching | breaks stdlib-only on the host side |
| stdlib socket to a long-lived backend | no | no | as above | one Unix-socket round trip; API call can be served from a cache | a daemon, a shared socket, a versioned protocol |

Where the client-go code lives (one module with tags, or a nested module) and how the core reaches it (linked in a second binary, exec'd, or over a socket) are independent choices: the out-of-process options still need one of the two layouts for the backend's own source.

## Aside

`nix/gomod2nix.toml` pins `github.com/onsi/ginkgo/v2` at `v2.28.1` while `go.mod` requires `v2.33.1` (and `google/pprof` differs too), so the checked-in toml was already out of step with `go.mod` when this was gathered; with two modules there are two such files to keep current.
