# Aperta

A coding agent in one small binary.

## Build

Needs Go 1.27 or later.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o aperta ./cmd/aperta
```

## Use

```sh
export ANTHROPIC_API_KEY=...
aperta run "explain what this project does"
aperta run --allow all "fix the failing test"
echo "list the TODOs" | aperta run --json
```

- `--allow read` (default) lets the agent use read, grep and glob only. `--allow all` also allows write, edit and shell.
- `--json` prints one event per line: `message_delta`, `tool_call`, `tool_result`, `usage`, `error`, `done`.
- `--model` (or `APERTA_MODEL`), `--effort`, `--max-turns`, `--max-tokens`, `--cwd`. See `aperta run -h`.

## License

[FSL-1.1-ALv2](LICENSE.md). Free to use, modify and self-host, including at work. You may not offer it as a competing product or service. Each release becomes Apache 2.0 two years after it ships.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Author

Made by CJ (Colin Jaxon).
