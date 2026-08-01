# wrap

Supervise persistent coding-agent terminals from one tmux workspace and
mirror any of them into an encrypted browser session visible from anywhere.

> **Status:** `v0.1.0-beta.4` is a pre-1.0 release. CLI and configuration
> behavior may change before 1.0.

wrap opens a folder, discovers its Git repositories and worktrees, and gives
you three panes: a repository tree, a persistent terminal, and a monitor of
every terminal in the workspace. When a program rings the terminal bell, its
session is marked with a 🔔 without moving the list.

Press `m` to pair a selected terminal with a phone or another browser through
an ephemeral Cloudflare tunnel with application-layer encryption.

<img width="1635" height="1079" alt="ghostty-desktop-wrap" src="https://github.com/user-attachments/assets/14d8489c-cb52-4437-8a02-742a341cd2d8" />
<img width="603" height="1136" alt="mobile-mirror" src="https://github.com/user-attachments/assets/dcb87251-bd17-45cc-89c6-c5d815c42e97" />

## Why wrap

- See which agent needs attention without cycling through terminal tabs.
- Take an interactive terminal to your phone or another browser through an
  encrypted, ephemeral mirror.
- Detach from the UI without stopping shells, agents, or long-running jobs.
- Use existing folders, repositories, and worktrees; wrap does not create or
  rearrange them.
- Repository and worktree topology is captured at launch; relaunch wrap after
  creating, removing, or moving a worktree.
- Start with no configuration and add only the behavior you want.
- Keep wrap's UI isolated from your ordinary tmux server and sessions.

wrap is program-agnostic. It does not parse agent output or require an agent
integration: any program that rings the terminal bell can request attention.

## Requirements

- tmux 3.2 or newer
- Git
- `less` (for file diffs)
- [`cloudflared`](https://developers.cloudflare.com/tunnel/downloads/) 2020.5.1
  or newer (required for encrypted browser mirroring)
- macOS or Linux

Go is needed only when installing from source. Ordinary local use does not
require `cloudflared`.

## Install

Install from the
[`sarcasticbird/homebrew-tap`](https://github.com/sarcasticbird/homebrew-tap):

```sh
brew install sarcasticbird/tap/wrap
wrap version
```

The formula installs tmux as a dependency. Encrypted browser mirroring still
requires `cloudflared`; install it separately as shown below.

Or install this beta from source with Go:

```sh
go install github.com/sarcasticbird/wrap/cmd/wrap@v0.1.0-beta.4
wrap version
```

`go install` writes to `$(go env GOPATH)/bin`; make sure that directory is on
your `PATH`.

To use encrypted browser mirroring, install `cloudflared` and make sure it is
also on `PATH`. On macOS:

```sh
brew install cloudflared
cloudflared --version
```

For Linux packages and binaries, use Cloudflare's
[official downloads](https://developers.cloudflare.com/tunnel/downloads/).

Prebuilt archives for macOS and Linux are attached to the
[`v0.1.0-beta.4` release](https://github.com/sarcasticbird/wrap/releases/tag/v0.1.0-beta.4).
Each archive contains `wrap`, `LICENSE`, and `README.md`; verify it against
`checksums.txt` before installing.

## Quick start

```sh
cd ~/Projects/my-projects
wrap
```

Or open another folder directly:

```text
wrap [directory]
```

The CLI is intentionally small:

```sh
wrap
wrap ~/Projects/my-projects
wrap tui
wrap version
```

Select a repository in the tree, focus the terminals pane, and press `n` to
bind or create its terminal. Press `q` when you are done watching; the
sessions continue running. Launch the same folder again to reattach.

### Switch between active workspaces

Run `wrap tui` to list every active Wrap workspace:

```text
Active Wraps

▸ apple-playlist  recover
  /Users/alex/Projects/apple-playlist
  coop  attached
  /Users/alex/Projects/coop
  wrap
  /Users/alex/Projects/wrap

enter attach · q quit
```

Use Up/Down or `j`/`k` to move, then press Enter to attach. Detaching from
that workspace returns to a freshly updated selector; `q` exits. `attached`
means a client is currently viewing the workspace. `recover` means work
sessions survived without UI chrome; selecting it runs the normal launch
validation and rebuilds the chrome automatically.

The selector displays each workspace's full saved root and reconciles saved
metadata with both live Wrap tmux servers, so stale metadata alone does not
create a row. A directory literally named `tui` can still be opened as
`wrap ./tui`.

## Layout and keys

The `Git (⌥2)` and `Terminals (⌥3)` panes share one column. The active
terminal (`⌥1`) occupies the full-height column beside them. Set
`tree_side = "right"` to mirror the layout. Custom focus bindings replace the
key labels shown in the headings.

| Key | Action |
| --- | --- |
| `j` / `k`, Up / Down | Move the cursor |
| Left / Right | Collapse / expand changed files or terminal PWD details |
| `Enter` | Select/open the current row or show the current session |
| `h` | Open Help in the focused list pane |
| `n` | Bind the selected tree row or create a scratch terminal |
| `m` | Mirror the selected non-diff terminal to an encrypted browser session |
| `r` | Rename a scratch terminal |
| `x` | Tree: kill the selected live entry; terminals: kill only a scratch terminal |
| `q` | Detach without stopping sessions |
| `Q` | Destroy only the current workspace's sessions after confirmation |
| `Option-1/2/3` | Focus active terminal / Git / Terminals |

Clicking moves the cursor; it does not activate a row. The tree never creates
a terminal. It records the selected folder, and `n` in the terminals pane
creates or binds the corresponding session.

Each list pane keeps the compact footer `h help · q detach · Q shutdown`.
While Help is open, press `h`, `Esc`, or `q` to close it; `q` does not detach
from Help.

The terminal monitor stays in creation order as names, bell state, and activity
indicators change. Repository, worktree, root, and diff rows are protected
there; only `·term·` scratch terminals may be renamed or killed. Expand a
terminal row to see the active pane's full current working directory. The
absolute path is shown when it fits; narrower panes truncate its left side
with an ellipsis so the most specific path components remain visible.

```text
  › repo
▸ ⌄ scratch
    /Users/alex/Projects/example
```

### Encrypted browser mirror

Focus a workspace-root, repository/worktree, or scratch row in Terminals and
press `m`. wrap starts a loopback-only web server and an ephemeral Cloudflare
Quick Tunnel, then shows a QR code and pairing URL. Scan it to open an
interactive xterm client on a phone or another browser. Diff terminals cannot
be mirrored. If the Terminals pane is too short for the complete pairing
composition, wrap grows only that pane and restores its exact previous height
when the overlay closes. Its adjacent Tree pane may temporarily shrink; the
full-height terminal pane, pane widths, zoom, and selection are unchanged. The
URL and a manual-enlarge hint remain available if the pane cannot grow; a QR
code is never rendered partially.

The phone viewer opens at a readable scale: it uses **Fit** when the terminal
width fits at 50% or larger, and otherwise starts at 50% with panning. The
terminal header keeps `Keyboard`, `Fit`, and `Close` reachable without consuming
a bottom row. `Keyboard` opens the software keyboard and horizontally
scrollable special-key row; its label becomes `Hide keyboard` so both can be
collapsed while typing. A tap on the terminal also enters typing mode, while a
one-finger drag pans without opening the keyboard. Two-finger pinch previews a
continuous manual scale from 30% through 200%, commits the terminal metrics when
the gesture ends, and briefly displays the current percentage. Press `Fit` to
fit the terminal width; a tall terminal remains vertically scrollable, and Fit
may go smaller than 30% for very wide sessions. Opening, fitting, pinching,
panning, rotating the phone, and opening the keyboard never resize the remote
tmux window.

The browser terminal implementation and its xterm.js assets are embedded in
the wrap binary. The host does not need an `xterm` executable or a separate
xterm package.

The URL fragment is the live shell credential: anyone who has it can control
every terminal currently mirrored from that workspace. The browser removes
the fragment from visible history immediately and retains it only in
tab-scoped session storage. A browser may clone that storage when a paired tab
is duplicated or creates another tab, so rotate the credential instead of
treating duplication as an independent pairing test. Terminal frames are
encrypted in the browser with AES-GCM using keys derived from that fragment;
the secret is never sent in an HTTP request, WebSocket header, query, or cookie.

Inside the overlay:

- Up / Down or `j` / `k` scroll pairing details in a short pane.
- `c` copies the complete pairing URL with `pbcopy` on macOS or an available
  `wl-copy`, `xclip`, or `xsel` helper on Linux. The URL is passed over stdin,
  not through process arguments or a tmux paste buffer.
- `h` shows the workspace diagnostics path, safe tail command, and privacy
  note; `Esc` closes Help before it closes the overlay.
- `x` revokes the selected terminal; revoking the last one stops the server
  and tunnel.
- `R` creates a new pairing credential and disconnects existing browsers.
- `Esc` closes the overlay (or cancels startup) without revoking a ready
  mirror.

Quick Tunnels are ephemeral and development-oriented: their hostname and
availability are not stable. Cloudflare terminates browser TLS and supplies
the encrypted client assets from wrap, so an actively compromised edge could
replace the page before browser-side encryption starts. The design protects
terminal frames from passive tunnel inspection, not from a host, browser, or
edge that can replace either endpoint. See
[Architecture and trust boundaries](docs/architecture.md) for the protocol
and lifecycle boundaries.

### tmux prefix and pane control

wrap runs its chrome on its own tmux server with the prefix remapped to
`C-q` (not tmux's default `C-b`), the status bar hidden, and mouse and
system-clipboard integration on. The keys in the table above are wrap's own;
everything tmux still provides is reached through the `C-q` prefix. Because the
status bar is hidden, there is no on-screen reminder of the prefix.

| Task | How |
| --- | --- |
| Move between panes | `C-q` then an arrow key |
| Resize a pane | Drag the pane border with the mouse, or `C-q` then `Ctrl`+arrow (one cell) or `Alt`+arrow (a larger step) |
| Scroll back / copy mode | Scroll the mouse wheel, or press `C-q` then `[`; leave with `q` or Escape |
| Copy a selection | Select with the mouse — the text is sent to your system clipboard. From the keyboard, copy mode follows your tmux copy table (emacs by default: `Ctrl-Space` starts the selection, `Alt-w` copies) |
| Any other tmux command | `C-q` then the usual tmux key |

`q` and `Q` in the table are handled by wrap's panes, not tmux, so they never
need the prefix.

## Configuration

Configuration is optional. Copy [the tested example](examples/wrap.toml) to
`~/.config/wrap/wrap.toml`, or start with no file at all.

```toml
walk_depth = 1
tree_side = "left"
tree_width = 25

[defaults]
cmd = ""
```

`cmd` is trusted configuration. wrap passes it to tmux's shell handling for
each new session; wrap does not parse or sandbox it.

See [Configuration](docs/configuration.md) for every setting, default,
validation rule, and key binding.

## Persistence and shutdown

Work sessions live on wrap's separate `tmux -L wrap` server. Closing the
terminal window or detaching the wrap UI does not ask tmux to stop them.
Operating-system process supervision still determines what survives logout,
reboot, or a killed tmux server.

- `q` detaches every client viewing this workspace's UI; work sessions keep
  running.
- `x` kills the named session shown in the confirmation.
- `Q` kills sessions owned by the current workspace and then closes its UI.

For the process model and direct tmux recovery commands, read
[Architecture](docs/architecture.md).

## Documentation

- [Configuration](docs/configuration.md)
- [Architecture and trust boundaries](docs/architecture.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Mobile mirror UAT](docs/mobile-mirror-uat.md)
- [Security policy](SECURITY.md)

## Development

The repository includes an optional Flox environment for its contributor
toolchain. Flox is convenient, not required.

```sh
flox activate
go test ./...
go test -race ./...
go vet ./...
./scripts/check-doc-links.sh
```

Without Flox, install Go 1.26+, tmux 3.2+, Git, and `less`, then run the same
commands directly.

## Security

wrap makes no network requests during ordinary local use. When you explicitly
start a mirror, it launches `cloudflared` to create a Quick Tunnel. It also
starts trusted configured commands and user programs that may use the network.
wrap reads the folders you open, enables OSC 52 clipboard writes inside its
tmux servers, and keeps sessions alive after UI detach. Treat opened
repositories as trusted: Git may run clean/process filters selected by their
configuration and attributes while wrap inspects working-tree changes.

Read [SECURITY.md](SECURITY.md) before using wrap with untrusted commands or
repositories.

## License

Apache-2.0. See [LICENSE](LICENSE).
