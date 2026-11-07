# Aperta

A coding agent in one small binary.

## Build

Needs Go 1.27 or later.

```sh
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o aperta ./cmd/aperta
```

Checks:

```sh
make test     # go vet and go test -race
make smoke    # the real binary in a real terminal, against real providers (needs an OpenRouter key)
```

## Use

```sh
aperta                       # terminal UI; asks you to connect a provider the first time
aperta -c                    # continue the latest session here
aperta -r 20261107-0334      # resume a session by id prefix
aperta sessions              # list sessions for this directory
aperta run "explain what this project does"
echo "list the TODOs" | aperta run --json
```

Connecting a provider:

```sh
aperta connect                         # which providers are connected
aperta connect openrouter              # paste a key; it is checked before it is saved
aperta connect openai-compatible --base-url http://host:8000/v1
aperta connect --remove openrouter     # delete a saved key
```

In the terminal UI, `/connect` does the same: pick a provider, paste the key, and the model picker for it opens. Keys are saved in `~/.aperta/credentials.json`, readable only by you, and are shown masked (`sk-or-…9f2c`). An environment variable such as `OPENAI_API_KEY` still wins over a saved key.

Other models:

```sh
aperta --provider openai                                       # gpt-6-sol by default
aperta --provider openrouter --model anthropic/claude-sonnet-5-5
aperta --provider ollama --model qwen3-coder                   # local, no key
aperta --provider lmstudio --model <model id>
aperta --provider openai-compatible --base-url http://host:8000/v1 --model <model id>
aperta -c --provider openai                                    # continue a session with another model
```

In the terminal UI, `/model` lists your connected providers and then that provider's live model list; type to filter, arrows to move, Enter to pick. OpenRouter shows only models that can use tools, with their price or "free". `/model openrouter` opens that list directly, and `/model openai/gpt-6-luna` or `/model gpt-6-astra` (same provider) switches straight away. The switch keeps the conversation, applies from the next turn, is saved in the session, and becomes this project's default for new sessions. `--provider` and `--model` still win.

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
