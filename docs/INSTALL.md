# Installation

## Prebuilt binary

Linux, macOS and Windows releases are available through the installer:

```bash
curl -fsSL https://raw.githubusercontent.com/enola-labs/enola/main/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
```

The installer writes `~/.local/bin/enola` and verifies the release checksum.

PyPI and RubyGems install the same binary:

```bash
pip install enola-cli
```

```ruby
gem "enola"
```

For Rails tasks and providers, use [`enola-rb`](RAILS.md).

## Upgrade

```bash
enola upgrade
```

Restart running MCP servers after an upgrade.

## Build from source

Building requires Go 1.26 or newer and a C compiler:

```bash
git clone https://github.com/enola-labs/enola.git
cd enola
go build -o enola ./cmd/enola
```

Continue with the [CLI reference](CLI.md), [MCP setup](MCP.md), or the
[first-change tutorial](FIRST-CHANGE.md).
