# wrap

Supervise persistent coding-agent terminals from one tmux workspace.

> **Status:** `v0.1.0-beta.1` is a pre-1.0 release. CLI and configuration
> behavior may change before 1.0.

wrap opens a folder, discovers its Git repositories and worktrees, and gives
you three panes: a repository tree, a persistent terminal, and a monitor of
every terminal in the workspace. When a program rings the terminal bell, its
session is marked with a 🔔 without moving the list.

<img width="1638" height="1002" alt="Screenshot 2026-07-29 at 8 47 47 AM" src="https://github.com/user-attachments/assets/00f561a5-f4f8-4d4a-b1b8-6ed8c2fa7789" />

## Why wrap

- See which agent needs attention without cycling through terminal tabs.
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
- macOS or Linux

Go is needed only when installing from source.

## Install

Install this beta with Go:

```sh
go install github.com/sarcasticbird/wrap/cmd/wrap@v0.1.0-beta.1
wrap version
```

`go install` writes to `$(go env GOPATH)/bin`; make sure that directory is on
your `PATH`.

Prebuilt archives for macOS and Linux are attached to the
[`v0.1.0-beta.1` release](https://github.com/sarcasticbird/wrap/releases/tag/v0.1.0-beta.1).
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
wrap version
```

Select a repository in the tree, focus the terminals pane, and press `n` to
bind or create its terminal. Press `q` when you are done watching; the
sessions continue running. Launch the same folder again to reattach.

## Layout and keys

The tree and terminal monitor share one column. The active terminal occupies
the full-height column beside them. Set `tree_side = "right"` to mirror the
layout.

| Key | Action |
| --- | --- |
| `j` / `k`, Up / Down | Move the cursor |
| Left / Right | Collapse / expand the selected terminal's PWD details |
| `Enter` | Select/open the current row or show the current session |
| `l` / `h` | Expand/collapse changed files in the tree |
| `n` | Bind the selected tree row or create a scratch terminal |
| `r` | Rename a scratch terminal |
| `x` | Tree: kill the selected live entry; terminals: kill only a scratch terminal |
| `q` | Detach without stopping sessions |
| `Q` | Destroy only the current workspace's sessions after confirmation |
| `Option-1/2/3` | Focus tree / terminal / terminals |

Clicking moves the cursor; it does not activate a row. The tree never creates
a terminal. It records the selected folder, and `n` in the terminals pane
creates or binds the corresponding session.

The terminal monitor stays in creation order as names, bell state, and activity
indicators change. Repository, worktree, root, and diff rows are protected
there; only `·term·` scratch terminals may be renamed or killed. Expand a
terminal row to see the active pane's current working directory. Paths inside
the opened workspace are shown relative to its root; outside paths remain
absolute.

```text
  › repo
▸ ⌄ scratch
    ./current/path
```

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

wrap itself makes no network requests, but it starts trusted configured
commands and user programs that may do so. It reads the folders you open,
enables OSC 52 clipboard writes inside its tmux servers, and keeps sessions
alive after UI detach. Treat opened repositories as trusted: Git may run
clean/process filters selected by their configuration and attributes while
wrap inspects working-tree changes.

Read [SECURITY.md](SECURITY.md) before using wrap with untrusted commands or
repositories.

## License

Apache-2.0. See [LICENSE](LICENSE).
