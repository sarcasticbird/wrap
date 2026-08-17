# Contributing to Wrap

Wrap is a small developer tool with a narrow boundary: one running Wrap shares
one exact tmux window. Contributions should preserve the source tmux session,
keep pairing credentials out of persistent state and ordinary management
output, and avoid growing Wrap into a command supervisor or workspace manager.

## Development environment

Supported development and release targets are Apple Silicon macOS and 64-bit
ARM/x86 Linux. Install [Flox](https://flox.dev/docs/install-flox/), clone the
repository, and run development commands through the checked-in environment:

```sh
git clone https://github.com/sarcasticbird/wrap.git
cd wrap
flox activate
```

The environment supplies tmux and bootstraps the exact Go patch pinned by
`go.mod`. Install cloudflared separately when you need a live Quick Tunnel;
unit and integration tests do not require it.

You can also prefix individual commands with `flox activate --` instead of
opening an activated shell. The examples below use that form so the toolchain
is explicit.

## Build and test

Format changed Go files and run the fast suite while iterating:

```sh
flox activate -- go fmt ./...
flox activate -- go test ./...
```

Before requesting review, run the same core checks used by CI:

```sh
flox activate -- ./scripts/check-doc-links.sh
flox activate -- ./scripts/test-doc-links.sh
flox activate -- golangci-lint run
flox activate -- go test -race ./...
flox activate -- go vet ./...
```

CI runs the tmux integration suite on Apple Silicon macOS and Linux. Tests may
skip platform capabilities that are genuinely unavailable, but a missing tmux
binary is not a meaningful integration result.

## Build and local UAT

Build into the Git-ignored `bin/` directory:

```sh
flox activate -- go build -o bin/wrap ./cmd/wrap
export PATH="$PWD/bin:$PATH"
flox activate -- wrap version
flox activate -- wrap doctor
```

Putting this checkout's absolute `bin/` path first ensures the UAT commands do
not accidentally exercise an older installed Wrap. A live browser test also
needs cloudflared on `PATH`. Use disposable tmux sessions and follow the
[mobile mirror UAT](docs/mobile-mirror-uat.md). Always confirm that `wrap
remove` stops the share without killing the source window or session.

## Repository map

- `cmd/wrap`: CLI parsing and human/JSON management output
- `internal/target`: exact tmux target discovery and guarded helper ownership
- `internal/instance`: names, records, leases, and stale-state reconciliation
- `internal/control`: private CLI-to-worker protocol
- `internal/share`: detached worker startup and shutdown ordering
- `internal/mirror`: tunnel, browser assets, encryption, and tmux viewers
- `internal/doctor`: read-only dependency and recovery reporting
- `scripts`: documentation checks and reproducible release tooling

The browser application is embedded from `internal/mirror/assets`. When
updating vendored browser code, update its provenance and license material as
well as the browser contract tests.

## Pull requests

Keep changes scoped and include tests for behavior changes. Human-readable
terminal output must escape control bytes; JSON should preserve machine-readable
values and must not expose pairing credentials unless the command explicitly
requests them.

In the pull request, describe the tmux lifecycle impact, security impact, and
manual UAT performed. Do not bundle a version tag or release publication into a
feature pull request; releases are a separate maintainer action after merge.

Security vulnerabilities should use
[GitHub private vulnerability reporting](https://github.com/sarcasticbird/wrap/security/advisories/new),
not a public issue or pull request.
