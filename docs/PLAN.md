# Aperta plan

## Principles

Aperta must never feel overwhelming. That was Prism's biggest problem. Every UI change follows these rules:

- **Calm by default**: one thing asks for attention at a time. No splash screens, mascots, sidebars or dashboards unless the user opens them.
- **Progressive disclosure**: a tool call shows as one short line. Full output stays collapsed until the user asks for it.
- **Every screen explains itself**: any prompt shows which keys to press. No hidden modes.
- **Few commands**: a small /help with a short list. A feature is added only when it earns its place on screen.
- **A quiet status line**: model, session and tokens in one dim line. No progress spam.
- **Safe defaults**: nothing destructive happens on Enter or by accident.

## Name

- Product: Aperta. Package and CLI: `aperta`.
- Domain: aperta.dev (bought). Site, docs and installer come later.

## License

FSL-1.1-ALv2 (Functional Source License, becomes Apache 2.0 two years after each release). See [LICENSE.md](../LICENSE.md).

Why:
- Free to use, modify and self-host, including at work.
- Nobody can ship a competing product or a hosted copy.
- Keeps a paid or hosted tier open for later.

## Contributors

A CLA, not a DCO: relicensing and commercial use need a license grant to the licensor. On GitLab it lives in [CONTRIBUTING.md](../CONTRIBUTING.md) plus a checkbox in the merge request template (`.gitlab/merge_request_templates/Default.md`).

## Hosting

- GitLab is the main repo, under the group `aperta` (looks free; to be claimed).
- A read-only push mirror goes to GitHub (e.g. `apertahq/aperta`). Its README points to GitLab for issues and merge requests.

## Clean room

- Written from scratch. No code copied from Prism (MIT, GitHub) or any other agent harness.
- [DEPENDENCIES.md](DEPENDENCIES.md) lists each dependency and its license.
- Allowed licenses: MIT, Apache-2.0, BSD, ISC. Avoid GPL and AGPL.

## To do soon

- [ ] Publish a placeholder `aperta` package on npm, with author `CJ (Colin Jaxon)`.
- [ ] Run a USPTO trademark check for "Aperta" (PLOS used the name for a research submission platform).
- [ ] Optional: buy aperta.sh.

## Status

- v0.1: agent loop, Anthropic provider, the 6 core tools and headless mode.
- v0.2: terminal UI, permissions (once, for this project, no) and sessions with resume.
- v0.3: OpenAI Responses, OpenAI-compatible providers (OpenRouter, Ollama, LM Studio, any server) and MCP over stdio and HTTP.
- After v0.3: `/model` to switch model mid-session, remembered per project.
- After v0.3: `/connect` and `aperta connect` to add API keys from inside Aperta.
- After v0.3: `/model` lists each provider's live models, filtered as you type. Checked against the real OpenRouter API.

v0.1 and v0.2 were tested by hand against the real Anthropic API. v0.3 passes its tests against fake OpenAI, Chat Completions and MCP servers; it has not run against the real OpenAI API or a local model yet.

## Architecture

Go, one static binary, terminal UI plus headless mode. See [ARCHITECTURE.md](ARCHITECTURE.md) for layers, layout, milestones and open questions.
