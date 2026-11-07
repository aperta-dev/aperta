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
aperta                       # terminal UI
aperta -c                    # continue the latest session here
aperta -r 20261107-0334      # resume a session by id prefix
aperta sessions              # list sessions for this directory
aperta run "explain what this project does"
echo "list the TODOs" | aperta run --json
```

Other models:

```sh
OPENAI_API_KEY=... aperta --provider openai                    # gpt-6-sol by default
OPENROUTER_API_KEY=... aperta --provider openrouter --model anthropic/claude-sonnet-5-5
aperta --provider ollama --model qwen3-coder                   # local, no key
aperta --provider lmstudio --model <model id>
aperta --provider openai-compatible --base-url http://host:8000/v1 --model <model id>
aperta -c --provider openai                                    # continue a session with another model
```

MCP servers:

```sh
aperta mcp add files npx -y @modelcontextprotocol/server-filesystem .
aperta mcp add --header 'Authorization: Bearer $DOCS_TOKEN' docs https://example.com/mcp
aperta mcp list
aperta mcp remove files
```

- **Permissions**: read, grep and glob always run. write, edit, shell and MCP tools ask first: yes once, yes for this project, or no. Project rules are saved in `~/.aperta/permissions.json`, never in the repo. Headless mode can't ask, so it denies those tools unless a project rule allows them or you pass `--allow all`.
- **Sessions** are saved as JSONL in `~/.aperta/sessions` (or `$APERTA_HOME/sessions`). `aperta run` also takes `-c` and `-r`.
- `--json` prints one event per line: `message_delta`, `tool_call`, `tool_result`, `usage`, `error`, `done`, each with `session_id`.
- **MCP servers** are saved in `~/.aperta/mcp.json` and load in every project. `$VARS` in `--env` and `--header` values expand when the server starts.
- `--provider` (or `APERTA_PROVIDER`), `--model` (or `APERTA_MODEL`), `--base-url`, `--effort`, `--max-turns`, `--max-tokens`, `--cwd`. See `aperta -h` and `aperta run -h`.

## License

[FSL-1.1-ALv2](LICENSE.md). Free to use, modify and self-host, including at work. You may not offer it as a competing product or service. Each release becomes Apache 2.0 two years after it ships.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Author

Made by CJ (Colin Jaxon).
