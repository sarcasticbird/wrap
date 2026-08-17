# Security Policy

## Supported versions

Wrap is pre-1.0. Only the latest release receives security fixes.

## Report a vulnerability

Use [GitHub private vulnerability reporting](https://github.com/sarcasticbird/wrap/security/advisories/new).
Include the affected version, reproduction, impact, and known mitigations. Do
not open a public issue for an undisclosed vulnerability.

## Trust boundaries

- A pairing URL is an interactive-shell credential. Anyone with the complete
  fragment can control that one shared terminal. Use `wrap regen` if it may
  have escaped and `wrap remove` when sharing is finished.
- Browser frames are protected with fragment-derived AES-256-GCM keys. The
  fragment is not sent in HTTP requests, logs, process arguments, or persisted
  host state.
- Cloudflare terminates TLS and serves the browser application. A malicious or
  compromised edge could replace that JavaScript and attack key establishment.
  Application encryption protects against passive intermediaries, not an
  active edge, compromised browser, or compromised host.
- The HTTP/WebSocket listener binds only to loopback and checks the exact Quick
  Tunnel origin. Handshakes, clients, messages, and queues are bounded.
- Browser input has the authority of the user running Wrap. Wrap is not a
  sandbox and needs no elevated privileges.
- Wrap targets an exact tmux socket, server generation, helper session, and
  window. Cleanup kills only its marked grouped helper. It never intentionally
  kills the source window or session.
- Instance records are non-secret, private metadata. Directories are mode 0700
  and records/control sockets are mode 0600. A matching local control response,
  not a PID alone, establishes worker ownership. A per-instance process-held
  lease distinguishes a live but unreachable worker from stale state even if a
  PID has been reused.
- Quick Tunnels are ephemeral and provide no uptime guarantee.
