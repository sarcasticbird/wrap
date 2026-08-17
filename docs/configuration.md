# CLI, state, and naming

Wrap has no configuration file and no configured command. Its complete public
command surface is:

```text
wrap [-n NAME]
wrap list [--json]
wrap show INSTANCE [--json]
wrap regen INSTANCE [--json]
wrap remove INSTANCE
wrap doctor [--json]
wrap version
```

`NAME` is a printable label up to 64 Unicode characters without slashes,
backslashes, control characters, or outer whitespace. Command words such as
`list` are valid names when supplied with `--name`.

Without a name, Wrap starts with the current directory basename and adds a
deterministic numeric suffix when another target already uses it. Management
selectors accept an exact name, an exact instance ID, or an unambiguous ID
prefix.

Running `wrap -n NEW` again in the same wrapped window renames that live
instance atomically when `NEW` is unused. It never starts a second tunnel or
redirects an existing name from another window.

`list --json` contains only non-secret data. `show --json` and `regen --json`
include the pairing URL because those commands explicitly request the active
credential.

## Environment paths

- State: `$XDG_STATE_HOME/wrap`, falling back to `~/.local/state/wrap`
- Runtime sockets: `$XDG_RUNTIME_DIR/wrap`, falling back to
  `<state>/runtime`

State is not an autostart mechanism. Workers and tunnels are not restored after
logout or reboot.

Private worker leases and control sockets live in the runtime directory.
Allowlisted, bounded JSONL diagnostics live under `<state>/diagnostics`; they
contain no pairing credential.
