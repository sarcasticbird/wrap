# Mobile mirror UAT

Use disposable tmux sessions and a current build made with the
[contributor workflow](../CONTRIBUTING.md#build-and-local-uat). The host needs
tmux 3.2+ and cloudflared 2020.5.1+.

## Existing-window path

1. Start an ordinary tmux session, open two windows, and split the second into
   two panes with recognizable content in each.
2. Select the second window and run `wrap -n mobile-uat`.
3. Return the source session to the first window.
4. Scan the QR code on a phone.
5. Confirm the browser immediately opens the second window—there is no picker.
6. Tap each pane and confirm tmux moves focus to the tapped pane. Confirm a
   drag still pans and a pinch still zooms without changing pane focus.
7. Tap Copy and drag across terminal text. Confirm releasing the drag stages
   the selection without opening a clipboard prompt or sending terminal input,
   and changes the button to Copy selected. Tap Copy selected and confirm the
   selection reaches the phone clipboard. Enter Copy again without selecting
   text and confirm Cancel copy exits selection mode.
8. Copy single-line and multiline text from another app, tap Paste, approve the
   browser's native paste prompt if shown, and confirm the exact text reaches
   the terminal. Enable Ctrl before pasting and confirm the pasted text remains
   unchanged and Ctrl is no longer active. Confirm empty and denied clipboard
   reads show useful feedback without sending terminal input.
9. Print both a plain HTTP or HTTPS URL and an OSC-8 hyperlink. Tap each link
   and confirm it opens in a new browser tab. Confirm dragging across a link
   does not open it.
10. Open More and confirm Keyboard, Fit terminal, Reconnect, and Close are
    available there. Toggle Keyboard and confirm More closes while the keyboard
    toolbar opens and remains dismissible through More. Confirm Fit and
    Reconnect also close the menu, and pinch still works.
11. Run `wrap regen mobile-uat`; confirm the old tab disconnects and the new QR
   works.
12. Run `wrap kill mobile-uat`; confirm the source session and both windows
   still exist.

Repeat on a custom socket (`tmux -S /tmp/wrap-uat.sock ...`) to verify exact
socket targeting.

## Outside-tmux path

1. From a directory that is safe to use for UAT, run `wrap -n bootstrap-uat`
   outside tmux.
2. Confirm an ordinary default-server tmux session attaches in that physical
   directory and leaves you at your normal shell after printing pairing data.
3. Detach and reattach with ordinary tmux commands.
4. Confirm the browser share survives detachment.
5. Kill the Wrap and confirm the tmux session survives.

## Security checks

- `wrap list --json` contains no pairing URL or secret.
- Process arguments contain no pairing credential.
- Instance JSON contains no pairing credential or public URL.
- The credential disappears from the browser address bar after loading.
- Killing or rotating one Wrap does not affect another. Start two disposable
  Wraps, run `wrap kill all`, and confirm both source tmux windows survive.

## Pairing recovery

Use synthetic credentials on a disposable share. Record the Wrap build, device,
OS, browser version, and exact error text, never the key or full pairing URL.

1. In the same paired tab, reload, use Reconnect, and background/resume Safari.
   Confirm the still-running share opens without another scan or `wrap regen`.
2. If "Pairing key missing" occurs, open the full current pairing URL in that
   same tab, not just a fresh tab. Confirm it reloads, removes the fragment from
   the address bar, and authenticates. For a controlled recovery check, remove
   only `wrap.mirror.v3.secret` from that tab's session storage and reload first.
3. Run `wrap regen mobile-uat`. Confirm the old URL cannot authenticate; open
   the new full URL in the affected tab and confirm it recovers there too.
4. Confirm missing and wrong keys cannot open the terminal, and `wrap kill`
   disconnects the browser without killing the source window.

The controlled storage-loss check tests recovery, not the cause of Safari losing
tab storage. Keep naturally occurring failures separate from injected failures.
