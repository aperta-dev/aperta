# Aperta plan

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

Both pass their tests against a fake API. Neither has run against the real API yet.

## Architecture

Go, one static binary, terminal UI plus headless mode. See [ARCHITECTURE.md](ARCHITECTURE.md) for layers, layout, milestones and open questions.
