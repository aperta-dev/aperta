# Security

## Reporting a vulnerability

Please report it privately, not in a public issue: use GitHub private vulnerability reporting at https://github.com/aperta-dev/aperta/security/advisories/new.

Include what you found, how to reproduce it, and the `aperta version` output. You will get a reply within a few days.

## How Aperta handles keys

- Saved keys are stored in `~/.aperta/credentials.json` with mode 0600, in a folder with mode 0700, on macOS and Linux. On Windows, Aperta does not set permissions itself; the file relies on your user profile's folder permissions.
- Keys are shown only masked (`sk-or-…9f2c`).
- Keys are never written to session files or logs, and never sent anywhere except the provider they belong to.
