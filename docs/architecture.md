# Architecture and trust boundaries

wrap is a local terminal UI over two dedicated tmux servers, Git subprocesses,
and small JSON state files. It has no daemon, container, database, or network
client.

## Workspace identity

Every invocation targets a folder:

```text
wrap [directory]
```

wrap resolves the folder to its physical absolute path before discovery, so
repository paths and persisted root metadata point at the real directory. The
workspace name is the sanitized basename of the lexical path the user opened.
Reopening through the same symlink alias therefore preserves that alias's
sessions and state; different alias basenames for one physical folder create
different workspace identities.

Two different folders can still share a basename. If a live UI already owns
that name with different metadata, wrap refuses to overwrite it and reports
the conflicting root. Open the folders through a common parent with
`walk_depth` high enough to discover both repositories, or shut down the first
workspace.

`wrap-home` is reserved. It is the shared last-resort session used when no
workspace session is available, so a lexical path with that basename cannot
be a workspace. Basenames containing `·term·` or ending in `·diff` are
also refused because those strings identify scratch-terminal and diff-session
namespaces; a basename ending in `·term` is refused for the same reason. Path
separators, invalid UTF-8, and non-printable characters are rejected before a
name reaches tmux or the state-file protocol.

## Two tmux servers

### Session server

```text
tmux -L wrap
```

This server owns work sessions. It reads the user's tmux configuration when
the server starts, so personal shell/session behavior can apply. wrap sets the
global options and indexed bell hook it needs without using the default tmux
socket.

The reserved `wrap-home` session keeps the server available and gives nested
clients a safe landing place.

### UI server

```text
tmux -f /dev/null -L wrap-ui
```

This server owns one three-pane chrome session per workspace. `/dev/null`
keeps personal tmux options such as pane indexes from changing wrap's fixed
layout. The panes are:

1. `wrap sidebar <workspace>` — repository tree and Git state.
2. `wrap attach <workspace>` — nested client attached to the session server.
3. `wrap watch <workspace>` — terminal monitor and attention state.

Pane subprocesses are long-lived. A chrome build marker, a fingerprint of the
resolved entry topology, and the resolved configuration are persisted; a
binary, topology, or configuration change rebuilds the UI
chrome so old pane processes do not survive an upgrade. Work sessions remain
on the other server. Focus key bindings are global to the UI server, so their
commands resolve pane indexes from each chrome session's own mirrored-layout
option at keypress time; rebuilding a right-sided workspace cannot retarget
the keys in a left-sided one. When configured focus keys change, wrap also
unbinds the superseded global keys before installing their replacements.
Upgrade cleanup considers both recorded global markers and the workspace's
prior chrome configuration, but is skipped when rebuilding the last UI session
created a fresh tmux server.

## Session names and ownership

For workspace `project`, wrap recognizes:

```text
project                 workspace-root session
project/api             named repository or worktree
project·term·1          scratch terminal
project·term·logs       renamed scratch terminal
project·diff            one-shot diff pager
```

Ownership checks require the exact workspace name or one of those delimiters.
The similar name `project-old` is not owned by `project`.
Repository/worktree components use a reversible `~xx` byte encoding for
punctuation, so names such as `foo.bar`, `foo bar`, and `foo_bar` cannot
collapse onto one tmux session.

On upgrade, wrap renames a live session from the older underscore format when
exactly one current row maps to it, and updates the persisted selection. If
multiple rows collapse to the same legacy name, or both old and new names are
live, launch stops before renaming anything and reports the conflicting
sessions. A live migration also waits for complete repository/worktree
discovery and refuses to reinterpret a saved selection as a different current
entry. New entry sessions carry wrap-owned tmux options recording both their
encoded identity and a base64 representation of their canonical source path.
Launch requires complete repository/worktree discovery before it validates or
publishes the name-to-path mapping, then checks every live entry against it and
accepts an existing marked session only when both values match. For an older
session without a path marker, wrap retrieves its live pane directory with a
separate framing-safe tmux query; paths containing tabs or newlines cannot
corrupt the session-list protocol. The directory must canonicalize to the
requested entry before wrap backfills both markers. The validated mapping is
persisted for the pane subprocesses. New entry creation must appear in that
mapping. Every entry switch revalidates the path, while every entry, scratch,
diff, and home switch or automatic attach targets a stable tmux session ID
rather than its reusable name. A removed repository therefore cannot silently
hand its still-running terminal to a different path
that later reuses the same row name. Ambiguous, undiscovered, or
path-conflicting sessions are left untouched for explicit recovery. The
identity marker also makes an interrupted legacy rename resumable. Kill
confirmation captures the same stable ID and a random per-server generation at
`x` time, then performs a generation-guarded kill and compares the displayed ID
at `y` time. Name reuse, rename, and a work-session server restart therefore
cannot retarget the action. The same generation check guards legacy migration,
entry-marker backfill, scratch renames, diff replacement, switches, automatic
attach, and shutdown sweeps. Post-bootstrap session creation is also one
generation-guarded tmux queue operation, so a server restart cannot leave a
new command running on an unconfigured replacement server after wrap reports
failure. A session ID is never treated as durable across server lifetimes. A chrome
schema bump restarts existing panes into the marker-aware implementation, so a
later launch cannot encode the same name twice.

The tree records selections but never creates a work session. `n` in the
terminals pane creates or binds the selected row. Diff sessions are temporary
and return the nested client to its prior tmux history when they exit.

## State and polling

State lives under:

```text
$XDG_STATE_HOME/wrap/<workspace>
```

with `~/.local/state/wrap/<workspace>` as the fallback. It contains workspace
metadata, current selection, the launch-validated entry-path map, and chrome
build parameters. Files are written atomically with mode `0600`; directories
use `0755`, subject to umask.

Launch also takes an advisory `launch.lock` in that directory before checking
or writing workspace metadata. It holds the lock until the workspace UI
session exists, forcing concurrent same-basename launches to recheck visible
ownership instead of both claiming an unused name.

The tree and monitor poll Git/tmux state every two seconds, scheduling the next
poll only after the current one completes so slow subprocesses cannot overlap
or apply out of order. A failed tmux poll keeps the last successful rows
instead of displaying an empty workspace and marks them stale until a later
poll succeeds. Malformed or unreadable selection state is reported separately:
fresh session, activity, and bell updates still apply while the last valid
selection remains highlighted. The monitor asks tmux which session the nested
client actually displays; persisted tree selection is not treated as display
ground truth.

The monitor keeps sessions in tmux creation order; rename, bell, activity,
selection, and active-pane changes do not reorder rows. Tmux's
`session_created` timestamp provides the primary key and the numeric stable
session ID breaks same-second ties. Left and Right collapse or expand the
selected session. An expanded row asks tmux for that session's active pane
working directory on the same poll cycle. The query is guarded by both the
stable session ID and the session-server generation, so a restarted server or
reused ID cannot apply a path to the wrong row. Paths inside the workspace are
not treated specially: the compact, unlabeled detail line displays the full
absolute path when it fits and truncates its left side with an ellipsis when
necessary. Failed first reads show `⚠ unavailable`; later failures retain and
mark the last successful value stale with `?`.

Ordinary rows omit a neutral status marker because `›` and `⌄` already express
collapsed and expanded state. Activity `!` and bell `🔔` markers appear only
while meaningful; no empty attention column is reserved.

Pane 3 reserves rename and kill actions for `·term·` scratch sessions.
Workspace-root, repository, worktree, and diff sessions remain visible and
selectable there but cannot enter rename or kill confirmation. The launcher
revalidates scratch identity before executing a pane-3 kill.

Both lists render a cursor-following viewport when their rows exceed pane
height. Expanded PWD details count as physical lines but are not separate
selectable rows. A pending kill confirmation temporarily anchors that target
in view, and mouse row coordinates are translated through the same viewport.

A failed Git query leaves the repository row in place but removes unavailable
branch/count/file details and marks that row with `⚠`. Git errors do not use
the tmux `rows stale` footer.

Git inspection disables repository-configured filesystem monitors, hooks,
optional index writes, external diff programs, and text converters. Git can
still invoke configured clean/process filters selected by repository
attributes when it examines working-tree content, so opened repositories are
a trust boundary.

## Bells and attention

The session server installs an indexed `alert-bell` hook that records attention
in a wrap-owned tmux option without replacing the user's hook array.

The monitor:

- keeps sessions in creation order and marks ringing ones `🔔`;
- tracks new activity with `!`;
- clears attention when the session is deliberately shown;
- updates the outer workspace title; and
- writes a nonblocking BEL directly to terminals attached to that workspace.

The direct TTY write is needed because nested tmux servers can consume bell
propagation. A manually pinned terminal title may ignore wrap's title updates;
the BEL remains the fallback signal.

## Clipboard

Both servers enable tmux clipboard forwarding. Copy-mode selection can reach
the terminal through OSC 52 when the terminal permits it. The same mechanism
allows any program in a wrap session to request a clipboard write.

## Detach, shutdown, and recovery

`q` detaches every client attached to that workspace's UI. It does not kill
work sessions.

After confirmation, `Q` takes the workspace mutation lock and publishes a
durable shutdown barrier. Creation, rename, selection, diff, and kill actions
that were waiting on the lock then refuse to run. Shutdown generation-checks
each owned session kill, lists the server again to verify the namespace is
empty, clears the selection, and only then closes that workspace's UI. Errors
are joined and shown before the UI is destroyed. A later explicit launch clears
the barrier while holding the same lock.

wrap itself makes no network calls. Configured commands and programs inside
sessions have the normal authority of the local user, including network
access.

The session server is usable without wrap:

```sh
tmux -L wrap ls
tmux -L wrap attach -t <session>
```

The UI server is similarly inspectable:

```sh
tmux -f /dev/null -L wrap-ui ls
```

See [Troubleshooting](troubleshooting.md) before killing either server.
