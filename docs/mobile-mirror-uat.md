# Mobile mirror UAT

Use this checklist with a candidate wrap binary on the host and a current
mobile browser. Run it against a disposable workspace when practical. The
host needs tmux and `cloudflared`; it does not need an `xterm` executable or a
separate xterm package.

## Setup

1. Confirm the exact candidate and dependencies:

   ```sh
   /path/to/wrap-uat version
   tmux -V
   cloudflared --version
   ```

2. Launch a workspace with the candidate, create or select an interactive
   terminal, and run `stty size`. Keep those rows and columns for comparison.
3. Make the Terminals pane too short for a QR, note its exact height, focus the
   terminal's row, and press `m`. The Terminals pane grows enough to show the
   complete QR. Its adjacent Tree pane temporarily shrinks, while the
   full-height terminal pane, pane widths, zoom, and selection remain unchanged.
   Press `Esc` and confirm both panes return to their exact prior heights.
   Reopen the overlay and scan the current QR code. If a
   credential was previously shared, press `R` first and scan the replacement.

## Pairing and packaged client

- The page advances from `Checking encryption…` to the mirrored-terminal list.
- Opening a terminal renders output and accepts input without any host xterm
  installation.
- Reloading the paired tab reconnects. Opening the fragment-free URL in an
  independently created fresh tab does not inherit the credential. Do not use
  Duplicate Tab or an opener-created tab for this check: browsers may clone
  the paired tab's session storage into those tabs.
- Pressing `R` disconnects the old tab; only the newly scanned URL pairs.
- Pressing `c` copies the complete pairing URL with the platform clipboard
  helper (`pbcopy` on macOS; `wl-copy`, `xclip`, or `xsel` on Linux). The URL
  does not appear in process arguments, diagnostics, terminal control
  sequences, or tmux buffers.
- Pressing `h` opens mirror help with the workspace, diagnostics path, safe
  `tail -n 40` command, and privacy note. `Esc` closes Help before it closes
  the pairing overlay.
- At every pane height, the QR is either completely visible or completely
  omitted. If tmux cannot provide enough height, the complete pairing URL and
  `Enlarge pane to show QR` remain usable; no clipped QR rows appear.

## Mobile viewport and keyboard

- The first open briefly shows `Opening terminal…`, then reveals the terminal
  without persistent dashed padding. Reopening produces the same layout.
- The header keeps `Keyboard`, `Fit`, and `Close` visible; there is no bottom
  utility rail consuming terminal height.
- A wide terminal starts at 50% with panning instead of opening as a tiny full
  canvas. If the terminal width fits at 50% or larger, it starts fitted; height
  may remain vertically scrollable.
  Pressing `Fit` explicitly fits the complete canvas width and may display less
  than 30%; a tall canvas remains vertically scrollable, and tmux separators
  can therefore be visible in that mode.
- A two-finger pinch previews continuous manual scale between 30% and 200%
  without visible per-frame xterm reflow. The percentage appears during and
  briefly after the gesture, content under the midpoint stays anchored, and the
  final crisp terminal scale is committed when the pinch ends as the first
  finger lifts. A Fit
  scale below 30% remains fitted until an outward pinch reaches the manual
  floor.
- Pinch can make the surface wider than the phone; one-finger horizontal and
  vertical panning remain available and text selection stays aligned.
- A single non-drag tap or `Keyboard` opens the software keyboard. The
  special-key row appears only while typing and scrolls horizontally.
- Dragging to pan while in view mode does not open the keyboard.
- While the keyboard is open, `Hide keyboard`, `Fit`, and `Close` remain
  reachable in the header. `Hide keyboard` collapses the keyboard and
  special-key row.
- Rotate the phone and open/close the keyboard. The terminal refits or preserves
  manual scale without changing its remote rows and columns.
- Run `stty size` again from the phone. It matches the host geometry captured
  before pairing.
- On the first open, the terminal never receives a frame with a different
  logical tmux window size. The initial render and a later reopen have the same
  full-height layout without dotted unused tmux canvas.
- A host window larger than tmux's remembered `80x24` manual dimensions opens
  at the captured host geometry; the log ends in `geometry_verified` rather
  than `geometry_failed` and `open_failed`.

## Lifecycle and diagnostics

- End or revoke the open terminal while the keyboard is visible. The keyboard
  closes when the browser leaves the terminal view.
- Press `x` in the host overlay. The selected terminal disappears remotely;
  revoking the last terminal stops the mirror.
- Inspect the workspace `mirror.log` described in
  [Troubleshooting](troubleshooting.md#browser-remains-on-checking-encryption).
  It contains lifecycle JSONL but no pairing fragment, terminal content,
  session identity, or raw subprocess error.
- The first open emits `geometry_preparing` followed by `geometry_verified`, or
  `geometry_corrected` and then `geometry_verified`. A `geometry_failed` event
  blocks the viewer instead of forwarding a wrongly sized first frame. Geometry
  fields contain dimensions and status-row counts only.

Record the host OS and architecture, phone/browser version, candidate checksum,
and any failed checklist item with the corresponding safe diagnostic events.
