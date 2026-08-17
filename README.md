# wrap

Wrap shares one tmux window as an encrypted browser terminal. It is deliberately
small: Wrap does not manage repositories, commands, panes, or coding agents.
If a terminal already runs inside tmux—whether you created it yourself or an
app such as GhostHub owns the session—Wrap shares that exact window and leaves
its lifecycle alone.

## Build

Wrap supports Apple Silicon macOS and 64-bit ARM/x86 Linux. It requires:

- tmux 3.2 or newer;
- cloudflared 2020.5.1 or newer; and
- [Flox](https://flox.dev/docs/install-flox/) when building this checkout.

On macOS, install the runtime dependencies with Homebrew:

```sh
brew install tmux cloudflared
```

On Debian or Ubuntu, install tmux with apt and install cloudflared from
[Cloudflare's official packages](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/).

Build the current checkout into the Git-ignored `bin/` directory:

```sh
git clone https://github.com/sarcasticbird/wrap.git
cd wrap
flox activate -- go build -o bin/wrap ./cmd/wrap
export PATH="$PWD/bin:$PATH"
wrap doctor
```

The Flox environment supplies the repository's Go toolchain and tmux. A
standalone Wrap binary never installs or upgrades runtime dependencies.

## Share a terminal

Inside tmux, run:

```sh
wrap
```

Wrap captures the current window, starts one detached sharing worker, prints a
pairing URL and QR code, and returns to your shell. Give the share a management
name with `wrap -n api`.

Outside tmux, the same command creates and attaches an ordinary session on your
default tmux server in the physical current directory. It uses your normal tmux
configuration and shell—there is no private Wrap tmux server. Detach and
reattach with normal tmux commands.

## Everyday commands

```text
wrap                     Share the current tmux window
wrap -n api              Share it with the management name "api"
wrap list                List running shares without credentials
wrap show api            Show the current pairing URL and QR code
wrap regen api           Rotate the credential and disconnect browsers
wrap remove api          Stop sharing without killing the source window
wrap doctor              Check dependencies and local state
```

Management selectors accept an exact name, exact instance ID, or unambiguous
ID prefix. `list`, `show`, `regen`, and `doctor` also support `--json` where
documented in the [CLI reference](docs/configuration.md).

Running `wrap` again in the same window shows the existing pairing details.
Running `wrap -n NEW` renames that live share when the name is unused. There is
deliberately no `wrap <command>` form: Wrap mirrors the terminal; it does not
execute or supervise the command inside it.

While sharing, `tmux ls` shows an ephemeral `__wrap_<id>` helper session. It is
grouped with the source session so it can stay pinned to the captured window.
Removing a Wrap kills only that helper and the sharing worker; the source
window and session keep running.

## Security boundary

The complete pairing URL is an interactive-shell credential. Anyone who has it
can type in the shared terminal. Browser frames are encrypted with keys derived
from the URL fragment, and the fragment is not sent in HTTP requests or
persisted by the host. Cloudflare can still replace the JavaScript delivered
through its edge, so this protects against passive tunnel inspection—not a
malicious edge, browser, or host.

Use `wrap regen` if a URL may have escaped and `wrap remove` when sharing is
finished. Read [SECURITY.md](SECURITY.md) for the complete trust boundary.

## Develop Wrap

[CONTRIBUTING.md](CONTRIBUTING.md) covers the Flox environment, test suite,
package map, local builds, mobile UAT, and pull-request expectations.

## More documentation

- [CLI, state, and naming](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Mobile mirror UAT](docs/mobile-mirror-uat.md)

## License

Apache-2.0. See [LICENSE](LICENSE).
