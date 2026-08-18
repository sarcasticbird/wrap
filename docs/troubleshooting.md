# Troubleshooting

Start with:

```sh
wrap doctor
```

It checks tmux, cloudflared, private directory permissions, live, stale, and
unreachable records, and old Wrap tmux servers without changing anything.

## Missing or old dependencies

Wrap requires tmux 3.2+ and cloudflared 2020.5.1+. Management commands remain
available without cloudflared so you can inspect or remove an existing worker.

```sh
tmux -V
cloudflared --version
```

## The target is already wrapped

Running `wrap` twice in the same window reports the existing pairing details.
`wrap -n NEW` renames that same live share when the name is unused. If control
fails while the worker still holds its lease, `wrap doctor` reports the record
as unreachable and Wrap does not replace it. When no worker holds the lease,
`list`, a new start, or `remove` reconciles the stale record and guarded helper.
PID existence alone is never treated as ownership.

## A `__wrap_...` session is visible

That is the active helper session. Do not use it as your work session. `wrap
remove NAME` removes it after guarded ownership checks and leaves your source
session/window running.

## Browser pairing fails

- Scan the current QR code from `wrap show NAME`.
- Make sure the whole URL fragment is present.
- After `wrap regen`, old browser tabs are intentionally invalid.
- Quick Tunnels are ephemeral and may be unavailable on restricted networks.

## Terminal size looks wrong

Wrap captures host geometry and does not resize the host window from the
browser. Use Fit or pinch/pan in the browser. Explicit host-side tmux
`window-size` changes remain authoritative.

## Recover work from an old release

The redesign never kills the legacy `tmux -L wrap` or `tmux -L wrap-ui`
servers. `wrap doctor` prints exact list and attach commands for any it finds.
Recover or finish the work there, then clean it up manually with tmux only when
you are ready.
