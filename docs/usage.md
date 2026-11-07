# Using Aperta

## Start

```sh
aperta              # terminal UI in the current folder
aperta -c           # continue the latest session here
aperta -r 2026110   # resume a session by the start of its id
aperta sessions     # list sessions for this folder
```

In the terminal UI, `/help` lists the keys and commands.

## Connect a provider

`/connect` lists every provider and whether it is connected. Pick one, paste the key, and the model list for it opens. The key is checked before it is saved.

The same from the command line:

```sh
aperta connect                     # show what is connected
aperta connect openrouter          # paste a key, hidden as you type
aperta connect openai-compatible --base-url http://localhost:8000/v1
aperta connect --remove openrouter
```

Ollama and LM Studio need no key; Aperta checks that the local server is running and tells you how to start it if not.

## Providers and models

| Provider | Key | Notes |
| --- | --- | --- |
| `anthropic` | `ANTHROPIC_API_KEY` | the default provider |
| `openai` | `OPENAI_API_KEY` | |
| `openrouter` | `OPENROUTER_API_KEY` | needs a model |
| `ollama` | none | local, needs a model |
| `lmstudio` | none | local, needs a model |
| `openai-compatible` | `APERTA_API_KEY` (optional) | any server, needs `--base-url` |

Each provider starts on the provider's default model when it has one. Otherwise pass a model.

```sh
aperta --provider openai
aperta --provider openrouter --model anthropic/claude-sonnet-5-5
aperta --provider ollama --model qwen3-coder
aperta -c --provider openai        # continue a session with another provider
```

`/model` lists your connected providers, then the chosen provider's live model list. Type to filter, use the arrows, press Enter. Ctrl+A shows older models that are hidden by default. `/model <provider>` opens one list directly, and `/model <provider>/<model>` switches straight away.

A switch keeps the conversation, applies from the next turn, and becomes this folder's default for new sessions. `--provider` and `--model` (or `APERTA_PROVIDER` and `APERTA_MODEL`) still win.

## Permissions

read, grep and glob always run. write, edit, shell and MCP tools ask first:

- **1** yes, once
- **2** yes, for this project (shell rules cover one program, such as every `go` command)
- **3** no

Project rules are saved in `~/.aperta/permissions.json`, never in the repo.

## Sessions

Each session is a JSONL file in `~/.aperta/sessions`. Resuming shows the last few exchanges; the footer shows how much of the model's context the conversation uses, with a warning at 80%.

## Keys

Saved keys live in `~/.aperta/credentials.json`, readable only by you. They are shown masked (`sk-or-…9f2c`) and never written to sessions or logs. An environment variable such as `OPENAI_API_KEY` wins over a saved key.

## MCP servers

```sh
aperta mcp add files npx -y @modelcontextprotocol/server-filesystem .
aperta mcp add --header 'Authorization: Bearer $DOCS_TOKEN' docs https://example.com/mcp
aperta mcp list
aperta mcp remove files
```

Servers are saved in `~/.aperta/mcp.json` and load in every project. `$VARS` in `--env` and `--header` values are filled in when the server starts, so tokens stay out of the file.

## Headless mode

```sh
aperta run "explain what this project does"
echo "list the TODOs" | aperta run --json
aperta run --allow all "fix the failing test"
```

Headless mode can't ask, so write, edit, shell and MCP tools are denied unless a project rule allows them or you pass `--allow all`. `aperta run` also takes `-c` and `-r`.

`--json` prints one event per line: `message_delta`, `tool_call`, `tool_result`, `retry`, `usage`, `error`, `done`. Each has `session_id`; errors carry a plain `message` and the raw `error`.

Other flags: `--base-url`, `--effort`, `--max-turns`, `--max-tokens`, `--cwd`. See `aperta -h` and `aperta run -h`.

## Checks

```sh
make test     # go vet and go test -race
make smoke    # the real binary in a real terminal, against real providers
```

`make smoke` needs an OpenRouter key (and uses an Anthropic key if present), from the environment or `~/.aperta`. It uses free models and the cheapest paid ones.
