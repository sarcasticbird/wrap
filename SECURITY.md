# Security Policy

## Supported versions

wrap is pre-1.0. Only the latest release receives security fixes. Development
snapshots and older releases are unsupported; a fix may require upgrading
across an incompatible CLI or configuration change.

## Report a vulnerability

Use GitHub's private vulnerability reporting route:

<https://github.com/sarcasticbird/wrap/security/advisories/new>

Include the affected version or commit, reproduction steps, impact, and known
mitigations. Do not open a public issue for an undisclosed vulnerability.

## Trust boundaries

wrap needs no elevated privileges. Ordinary local use performs no network
requests; an explicitly started browser mirror launches `cloudflared` and
accepts encrypted terminal input through its Quick Tunnel. wrap also starts
other programs, queries repositories, and operates persistent tmux servers:

- **A mirror URL is an interactive-shell credential.** Anyone with the full
  URL fragment can control every terminal currently shared by that workspace.
  Do not post, log, or screenshot it. Revoke the final terminal or rotate the
  credential with `R` if it may have escaped.
- **Cloudflare is an active endpoint.** Browser-to-host frames use
  fragment-derived AES-GCM keys, so the credential does not enter an HTTP
  request and passive tunnel inspection cannot read terminal traffic.
  Cloudflare terminates TLS and can replace the browser page before encryption
  starts, so the mirror does not protect against a malicious or compromised
  edge, host, or browser. Quick Tunnels are ephemeral and have no uptime
  guarantee.
- **The mirror exposes only a loopback listener locally.** The server binds
  `127.0.0.1`, checks the exact Quick Tunnel origin, limits handshakes,
  clients, messages, and queues, and serves no health or debug endpoint.
  `cloudflared` remains a separately installed program running with the user's
  permissions.
- **Configured commands are trusted.** `[defaults].cmd` is passed to tmux's
  shell handling in each selected directory. wrap does not parse, inspect, or
  sandbox it. A configured command—and any program you start inside a
  session—can use the network and exercise your user's permissions.
- **Opened repositories are trusted Git inputs.** wrap discovers repositories
  and runs local Git status, diff, and worktree queries in the folder you open.
  Automated queries disable repository-configured filesystem monitors and
  hooks and suppress optional index writes; status statistics and the built-in
  diff pager also disable external diff and text-conversion programs. Git can
  still run configured clean/process filters selected by repository
  attributes while it inspects working-tree content. Do not open a repository
  whose Git configuration and attributes you do not trust.
- **Sessions persist after UI detach.** `q`, terminal closure, and loss of the
  wrap UI do not stop the separate session server. They do not promise
  survival across logout, reboot, or external process termination.
- **Shutdown is deliberately destructive.** After confirmation, `Q` kills
  only sessions in the current workspace's exact naming namespace, clears its
  selection, and closes its UI. `x` kills only the selected live session after
  confirmation.
- **OSC 52 clipboard writes are enabled.** Programs in either wrap tmux server
  can request writes to the host clipboard when the terminal permits OSC 52.
  This enables copy-out and also gives session programs clipboard-write
  authority without a manual selection.
- **State can contain the configured command.** Selection, workspace paths,
  and chrome parameters—including `[defaults].cmd`—are stored under
  `$XDG_STATE_HOME/wrap/`, falling back to `~/.local/state/wrap/`. Files are
  created with mode `0600`; parent directories use mode `0755`, both subject
  to the user's umask. Do not embed tokens or other credentials in `cmd`.
- **The session server reads your tmux configuration.** Work sessions use
  `tmux -L wrap`, so the user's tmux configuration applies when that server
  starts. The UI chrome instead uses
  `tmux -f /dev/null -L wrap-ui` to keep pane geometry and bindings isolated
  from the user's tmux configuration.

wrap keeps its two sockets separate from the default tmux server. That is
state and configuration separation, not a security sandbox.
