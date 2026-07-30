# Troubleshooting

## Browser mirror does not start

The mirror is optional and requires `cloudflared` 2020.5.1 or newer on
`PATH`. Check it from the same environment that launches wrap:

```sh
cloudflared --version
```

Press `m` again after correcting the installation. wrap uses a Cloudflare
Quick Tunnel and does not create, edit, or remove named-tunnel configuration.
An existing Cloudflare configuration can prevent Quick Tunnel startup; the
overlay reports a retryable failure without changing that configuration.
Startup also fails after 30 seconds if `cloudflared` cannot obtain a Quick
Tunnel URL. Confirm the host can make the outbound Cloudflare connections
allowed by your local firewall or network policy; wrap never opens a public
listener itself.

If the QR page opens but pairing fails, rotate with `R` and scan the new URL.
The fragment is intentionally removed from the address bar and kept only for
that browser tab. Reloading the same tab can reconnect; a different tab without
the fragment cannot. A rotated or revoked credential is rejected and stops
reconnecting. Quick Tunnel URLs are ephemeral and have no uptime guarantee, so
an unexpected tunnel exit revokes the workspace mirror and requires a fresh
`m`.

Use the HTTPS Quick Tunnel URL shown by wrap. Loading the loopback page
directly is unsupported because browser WebCrypto requires a secure context.
If the page reports an incompatible encrypted client, update to a browser with
WebCrypto support and rescan; wrap's crypto self-test intentionally connects
only after it passes.

If an open remote terminal ends, disappears from a later poll, or belongs to a
restarted tmux server generation, wrap closes that viewer instead of attaching
to a reused session identity. Return to the remote list and select a terminal
that is still mirrored.

## wrap says tmux is missing or too old

wrap requires tmux 3.2 or newer.

```sh
tmux -V
brew install tmux        # macOS
sudo apt install tmux    # Debian/Ubuntu
```

Install or upgrade tmux, then launch the workspace again.

## Option/Alt focus keys do not arrive

The default focus bindings are `M-1` for the active terminal, `M-2` for Git,
and `M-3` for Terminals. Your terminal must send Option/Alt as Meta.

For Ghostty on macOS:

```text
macos-option-as-alt = true
```

Or replace the bindings with tmux key syntax in
[configuration](configuration.md#focus-keys).

## tmux keys do nothing, or I cannot resize or scroll

wrap remaps the tmux prefix to `C-q` and hides the status bar, so the default
`C-b` does nothing and there is no on-screen reminder. Reach tmux with `C-q`:
a bare arrow key switches panes, `C-q` then `Ctrl`+arrow (or `Alt`+arrow for a
larger step) resizes a pane, and `C-q` then `[` (or the mouse wheel) enters
copy mode for scrollback. See
[Layout and keys](../README.md#tmux-prefix-and-pane-control) for the full list.

## Copy mode does not update the clipboard

wrap enables tmux clipboard forwarding, but the terminal can still reject OSC
52 writes. Enable clipboard writes in the terminal. For Ghostty:

```text
clipboard-write = allow
```

Hold Shift while dragging when you want a native terminal selection instead
of tmux copy mode.

## The agent finishes but no bell appears

wrap reacts only when the program emits a terminal BEL. It does not parse
output or infer completion.

1. Run the configured command manually in the same folder.
2. Confirm its notification setting emits a terminal bell.
3. Test the terminal path with `printf '\a'`.
4. Check the monitor footer for a stale tmux poll or alert-delivery error.

## The tab title stays on my manual name

Some terminals pin a title after you rename a tab and ignore later title
updates from applications. wrap cannot query or override that terminal-local
choice. The terminal BEL remains the attention signal; create an unpinned tab
if you also want `🔔 wrap: <workspace>` titles.

## Rows say they are stale or a repository shows ⚠

The tree and monitor retain their last successful session rows after a tmux
read fails. The footer reports `rows stale` and the cause. A failed selection
read is reported separately while fresh sessions continue updating.

A failed Git query keeps the repository row but removes unavailable
branch/count/file details and marks that row with `⚠`.

- Confirm the target repository is readable.
- Run `git status` in the affected repository.
- Inspect the session server with `tmux -L wrap ls`.
- Relaunch the workspace after correcting the error.

The stale marker or row warning clears after a successful poll.

## PWD details say unavailable or show `?`

Press Right on a terminal row to show the active pane's current working
directory. `⚠ unavailable` means the first generation-safe tmux read failed.
A trailing `?` means wrap retained the last successful path after a later
failure.

Confirm the session still exists with `tmux -L wrap ls`. A successful later
poll updates the path and clears the marker. Left collapses the detail without
changing or stopping the session.

## A same-basename workspace is refused

Live workspace identity uses the lexical folder basename. For example,
`~/a/api` and `~/b/api` both want the name `api`.

Shut down the existing `api` workspace with `Q`, or open a common parent and
set `walk_depth` high enough to discover both repositories as separate rows
inside one workspace. For `~/a/api` and `~/b/api`, that means at least
`walk_depth = 2`. wrap refuses to reuse live chrome with different root
metadata. It also refuses takeover while any session owned by the old
workspace remains, even if its UI has already disappeared. Inspect those
sessions with `tmux -L wrap ls`; attach to preserve work or kill the exact old
workspace sessions before retrying.

## An entry session belongs to a different path

wrap records each repository/worktree terminal's canonical source path. If a
row name is reused after its old directory is moved or removed, wrap refuses
to attach the surviving terminal to the replacement path. This check runs
across all live entries during launch and again immediately before wrap
switches or automatically attaches to an entry session.

Inspect the named session with `tmux -L wrap ls` and preserve anything still
running there. Then kill that exact stale session or shut down the workspace.
Relaunch wrap so discovery republishes the validated entry-path map before
selecting the replacement row and pressing `n`.

## A folder named wrap-home is refused

`wrap-home` is the session server's reserved fallback session. Rename the
folder or open its parent as the workspace.

The same recovery applies to the uncommon basenames containing `·term·` or
ending in `·diff`; wrap reserves those strings for scratch-terminal and
diff-session namespaces.

## A workspace folder name containing `$` is refused

tmux 3.2 through 3.4 cannot preserve a dollar sign in a session name. To keep
workspace identity consistent across every supported tmux version, wrap
refuses `$` in the folder opened as the workspace root. Rename that folder or
open its parent instead. Repository and worktree names inside the workspace may
still contain `$`; wrap encodes those session components.

## A configuration change rebuilt the UI

This is expected. Layout, keys, discovery depth, default command, and chrome
build changes require new tree/attach/watch pane processes. wrap rebuilds only
the UI chrome; work sessions on `tmux -L wrap` keep running.

Another client attached to the old UI is detached. Launch the same folder to
reattach.

## The configured command exits immediately

A non-empty `[defaults].cmd` replaces the login shell for new sessions. If it
is missing, rejects the folder, or exits, the session disappears.

1. Set `cmd = ""` and confirm a login shell works.
2. Run the intended command manually in the selected folder.
3. Restore `cmd` after fixing its PATH, arguments, or local configuration.

## The session server was lost or killed

wrap recreates the server, `wrap-home`, and required options on the next
launch or new-terminal action. Processes from the killed server cannot be
recovered by wrap; start replacement sessions after relaunch.

## `wrap tui` reports workspace metadata or recovery errors

The selector lists a workspace only when saved metadata supplies a trusted
absolute root and live tmux state proves its chrome or work sessions still
exist. A metadata warning means live tmux state exists but
`$XDG_STATE_HOME/wrap/<workspace>/workspace.json` (or the fallback under
`~/.local/state/wrap`) is missing, malformed, or invalid.

Inspect the live state without changing it:

```sh
tmux -L wrap-ui ls
tmux -L wrap ls
```

Do not invent or copy workspace metadata from another folder. If the saved
root still exists, launch that exact folder normally so wrap can validate and
publish current metadata:

```sh
wrap /absolute/path/to/workspace
```

Selecting a `recover` row performs that same normal launch automatically. If
the root was moved or deleted, restore it at the recorded path or preserve and
finish the surviving work through direct tmux access before stopping those
sessions. A transient `rows stale` message retains the last good selector
rows but disables Enter until a successful poll. If a workspace exits between
display and selection, wrap returns to the selector with a launch error instead
of starting a new workspace.

## Access sessions without wrap

List and attach directly:

```sh
tmux -L wrap ls
tmux -L wrap attach -t <session>
```

Use exact session names from the list. This is also the recovery path when the
UI server is unhealthy.

## Launch reports an ambiguous legacy session

Older development builds replaced spaces, dots, and colons with underscores in
repository session names. When exactly one current row owns a live legacy
session, wrap migrates it automatically. If two rows would have shared that
name, wrap cannot safely decide which work the session belongs to and leaves
all sessions untouched.

Use `tmux -L wrap ls` and attach to the reported exact session name. Preserve or
finish that work, then stop the conflicting legacy session and launch wrap
again. The same recovery applies when both the legacy and encoded names are
already live, when a saved historical entry collides with a different current
entry, or when repository/worktree discovery is incomplete. Resolve a reported
discovery error before retrying migration. wrap does not publish a partial
entry-path map, because treating omitted rows as absent could misclassify a
still-running session.

## Detach versus shutdown

- `q` detaches every client viewing this workspace's UI. Work continues.
- Closing the terminal window also leaves tmux work sessions alone, subject to
  normal operating-system process lifetime.
- `x` in the tree confirms one selected live entry; `x` in terminals confirms
  only a `·term·` scratch session.
- `Q` confirms, kills only the current workspace's owned sessions, clears its
  selection, and closes that workspace's UI.

Choose `q` unless you intend to stop the workspace's processes.
