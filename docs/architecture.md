# Architecture

Wrap has no central daemon. Every shared terminal has one detached worker and
one private Unix control socket.

## Target and ownership

Inside tmux, Wrap reads `TMUX` and addresses its exact socket path with `tmux
-S`. It records the server generation, stable session ID, stable window ID,
and current directory. This works with the default server, named servers, and
servers owned by other terminal applications.

The worker creates an ephemeral session containing one link to the authorized
window and no fallback windows. The helper can remain on that window without
moving the user's session, and destroying the window also destroys the helper.
Wrap marks the helper with its instance ID and disables its prefix keys and
status line.

Cleanup requires the original socket, generation, helper ID, helper name, and
ownership marker to match. It kills the helper only. A linked window remains
alive in the source session.

## Worker resources

One worker owns:

- the single-window tmux helper;
- a loopback HTTP/WebSocket server;
- one cloudflared Quick Tunnel;
- an in-memory 32-byte pairing credential;
- independent PTY-backed tmux viewers for connected browsers; and
- a mode-0600 Unix control socket and advisory worker lease.

The public CLI starts the worker by re-executing the same binary. Launch state
travels over an inherited pipe, and the parent returns only after it receives a
matching ready status. Pairing material is never placed in argv or an instance
record.

## Browser protocol

Protocol v3 authenticates with the URL-fragment credential, derives separate
AES-256-GCM keys by direction with HKDF-SHA-256, and uses monotonic nonce
counters. After authentication, the server automatically opens the one target
and sends its stable identity and captured geometry. There is no terminal list
or target-selection request.

Each browser gets its own ignored-size tmux client. Browser fitting and panning
do not resize the host window. Window-size pinning is generation guarded and
restored only while Wrap still owns the temporary value.

## Local state

Non-secret instance records are under `$XDG_STATE_HOME/wrap/instances`, with
`~/.local/state/wrap/instances` as the fallback. Control sockets prefer
`$XDG_RUNTIME_DIR/wrap` and otherwise use the state root's `runtime` directory.
Directories are mode 0700 and records/sockets are mode 0600.

Records contain the name, worker PID, control path, start time, display
directory, and tmux identity. `list`, `show`, `regen`, and `remove` resolve a
record and then require a matching response from its control socket; PID
existence alone is never authority. Each worker also holds an instance-ID lease.
If control is unreachable, a held lease preserves the record as unreachable;
an acquirable lease proves the worker is gone even when its PID was reused, so
Wrap can safely remove stale state and guarded worker artifacts.

Bounded, allowlisted JSONL diagnostics are written under the private state root.
They exclude pairing credentials, encrypted frames, and raw subprocess output.

## Failure boundaries

A vanished target, changed tmux generation, tunnel exit, signal, or explicit
remove closes viewers and worker-owned resources. It does not send
`kill-window` or mutate the source session. Failure of one Wrap does not affect
another.
