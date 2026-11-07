# Security

## Reporting a vulnerability

Please report it privately, not in a public issue: open a confidential issue at https://gitlab.com/aperta-dev/aperta/-/issues/new (tick "This issue is confidential").

Include what you found, how to reproduce it, and the `aperta version` output. You will get a reply within a few days.

## How Aperta handles keys

- Saved keys are stored in `~/.aperta/credentials.json` with mode 0600, in a folder with mode 0700.
- Keys are shown only masked (`sk-or-…9f2c`).
- Keys are never written to session files or logs, and never sent anywhere except the provider they belong to.
