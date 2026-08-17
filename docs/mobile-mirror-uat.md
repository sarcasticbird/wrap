# Mobile mirror UAT

Use disposable tmux sessions and a current build made with the
[contributor workflow](../CONTRIBUTING.md#build-and-local-uat). The host needs
tmux 3.2+ and cloudflared 2020.5.1+.

## Existing-window path

1. Start an ordinary tmux session and open two windows.
2. Select the second window and run `wrap -n mobile-uat`.
3. Return the source session to the first window.
4. Scan the QR code on a phone.
5. Confirm the browser immediately opens the second window—there is no picker.
6. Type, use Ctrl keys, scroll, Fit, pinch, and reconnect.
7. Run `wrap regen mobile-uat`; confirm the old tab disconnects and the new QR
   works.
8. Run `wrap remove mobile-uat`; confirm the source session and both windows
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
5. Remove the Wrap and confirm the tmux session survives.

## Security checks

- `wrap list --json` contains no pairing URL or secret.
- Process arguments contain no pairing credential.
- Instance JSON contains no pairing credential or public URL.
- The credential disappears from the browser address bar after loading.
- Removing or rotating one Wrap does not affect another.
