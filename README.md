# Aperta

A calm coding agent for your terminal.

Aperta reads, edits and runs code in your project through a small set of tools, and asks before it changes anything. It is one static binary that works with Anthropic, OpenAI, OpenRouter and local models. It aims to stay quiet and clear rather than feature-heavy.

**Status:** in development, not released yet.

## Build from source

Needs Go 1.27 or later.

```sh
git clone https://gitlab.com/aperta-dev/aperta.git
cd aperta
make build
```

## Quickstart

```sh
aperta                  # open the terminal UI in your project
/connect                # add an API key or a local model server
/model                  # pick a model
aperta -c               # continue the latest session here
aperta run "explain this project"   # one task, no UI
```

## Docs

- [Usage](docs/usage.md): providers, models, MCP, sessions, permissions, headless mode
- [Architecture](docs/ARCHITECTURE.md): how Aperta is built
- [Contributing](CONTRIBUTING.md) and [Security](SECURITY.md)

## License

[FSL-1.1-ALv2](LICENSE.md): free to use, modify and self-host, including at work, but not to offer as a competing product or service.
Each release becomes Apache 2.0 two years after it ships.

Development happens on GitLab; GitHub is a read-only mirror. Made by CJ (Colin Jaxon).
