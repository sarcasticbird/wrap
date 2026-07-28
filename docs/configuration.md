# Configuration

wrap works without a configuration file. When present, the file is:

```text
$XDG_CONFIG_HOME/wrap/wrap.toml
```

If `XDG_CONFIG_HOME` is unset, wrap uses:

```text
~/.config/wrap/wrap.toml
```

An absent or empty file is valid. Start from the
[parser-backed example](../examples/wrap.toml) when you want explicit
settings.

## Complete example

```toml
walk_depth = 1
tree_side = "left"
tree_width = 25

[defaults]
cmd = ""

[keys]
focus_tree = "M-1"
focus_terminal = "M-2"
focus_terms = "M-3"
```

Unknown keys produce a warning and are ignored. Invalid recognized values,
such as `tree_side = "up"`, stop launch with an error.

## Layout and discovery

`walk_depth`, `tree_side`, and `tree_width` are accepted either at the top
level or inside `[defaults]`. A top-level value wins when both positions are
present. This compatibility does not apply to `cmd`, which belongs in
`[defaults]`.

### `walk_depth`

- Default: `1`
- Accepted result: clamped to `1..5`

This is the number of directory levels wrap examines during repository
discovery. The directory scan stops descending once it finds a plain repository
or a directory containing `.bare`. A plain repository keeps its own row; wrap
then queries `git worktree list` and adds linked worktrees that do not already
have rows. A `.bare` container is expanded directly into its non-bare
worktrees.

### `tree_side`

- Default: `"left"`
- Values: `"left"` or `"right"`

`left` places the tree and terminal monitor to the left of the terminal.
`right` mirrors the columns. Any other value is an error.

### `tree_width`

- Default: `25`
- Accepted result: clamped to `10..60`

The value is the percentage of the window assigned to the tree/monitor
column. The terminal receives the remaining width. Explicit zero is a real
value and clamps to `10` whether it is written at the top level or inside
`[defaults]`; only an omitted value receives `25`.

## New-session command

```toml
[defaults]
cmd = ""
```

An empty value starts the user's default shell. A non-empty value is passed to
tmux as the shell command for each new work session:

```toml
[defaults]
cmd = "codex"
```

This value is trusted. wrap does not tokenize, validate, or sandbox it. Test
the command manually in the target folders before making it the default.
The value is persisted in wrap's mode-`0600` chrome state so configuration
changes can rebuild the UI; do not embed tokens or other credentials in it.

The tree does not start commands. It records a selection; pressing `n` in the
terminals pane starts or binds that selection using `cmd`.

Terminal PWD details, creation ordering, and scratch-only lifecycle protection
require no configuration. Use
Left and Right in the terminals pane to collapse or expand the selected row.
wrap does not inspect process environments or infer tool-specific state.

## Focus keys

```toml
[keys]
focus_tree = "M-1"
focus_terminal = "M-2"
focus_terms = "M-3"
```

Values use tmux key syntax. The defaults are `M-1`, `M-2`, and `M-3`.
On macOS, `M-1` is usually Option-1 when the terminal is configured to send
Option as Alt/Meta.

## Applying changes

Configuration is loaded at launch. If a layout, key, command, discovery, or
chrome build value differs from the live UI, wrap rebuilds only that
workspace's UI chrome and starts new pane subprocesses. Work sessions on
`tmux -L wrap` are preserved.

Another client attached to the rebuilt UI chrome is detached and can reattach
by launching the workspace again.
