# Dependencies

Every dependency and its license. Allowed: MIT, Apache-2.0, BSD, ISC. Avoid GPL and AGPL.

| Package | Version | License | Why |
| --- | --- | --- | --- |
| Go standard library | 1.27 | BSD-3-Clause | Everything else, including the provider HTTP clients |
| charm.land/bubbletea/v2 | v2.1.1 | MIT | Terminal UI event loop and renderer |
| charm.land/bubbles/v2 | v2.2.1 | MIT | Text input and spinner widgets |
| charm.land/lipgloss/v2 | v2.0.6 | MIT | Terminal colours and styles |
| github.com/modelcontextprotocol/go-sdk | v1.8.0 | Apache-2.0 (older parts MIT) | MCP client, stdio and streamable HTTP |
| github.com/charmbracelet/x/term | v0.2.2 | MIT | Hidden key input for `aperta connect` |

Pulled in by those libraries (indirect, linked into the binary):

| Package | Version | License |
| --- | --- | --- |
| github.com/atotto/clipboard | v0.1.4 | BSD-3-Clause |
| github.com/charmbracelet/colorprofile | v0.4.3 | MIT |
| github.com/charmbracelet/ultraviolet | v0.0.0-20261008173134-6b8d4baf91b4 | MIT |
| github.com/charmbracelet/x/ansi | v0.11.9 | MIT |
| github.com/charmbracelet/x/term | v0.2.2 | MIT |
| github.com/charmbracelet/x/termios | v0.1.1 | MIT |
| github.com/charmbracelet/x/windows | v0.2.2 | MIT |
| github.com/clipperhouse/displaywidth | v0.11.0 | MIT |
| github.com/clipperhouse/uax29/v2 | v2.7.0 | MIT |
| github.com/google/jsonschema-go | v0.4.3 | MIT |
| github.com/lucasb-eyer/go-colorful | v1.4.1 | MIT |
| github.com/mattn/go-runewidth | v0.0.30 | MIT |
| github.com/muesli/cancelreader | v0.2.2 | MIT |
| github.com/rivo/uniseg | v0.4.7 | MIT |
| github.com/segmentio/asm | v1.1.3 | MIT |
| github.com/segmentio/encoding | v0.5.4 | MIT |
| github.com/xo/terminfo | v1.0.0 | MIT |
| github.com/yosida95/uritemplate/v3 | v3.0.2 | BSD-3-Clause |
| golang.org/x/oauth2 | v0.35.0 | BSD-3-Clause |
| golang.org/x/sync | v0.22.0 | BSD-3-Clause |
| golang.org/x/sys | v0.47.0 | BSD-3-Clause |
| golang.org/x/time | v0.15.0 | BSD-3-Clause |

Checked with `go version -m` on the built binary. Test-only and unlinked modules in go.sum are not listed.

Used only by `make smoke` (`tools/smoke`), never linked into the `aperta` binary:

| Package | Version | License | Why |
| --- | --- | --- | --- |
| github.com/creack/pty | v1.1.24 | MIT | Runs the real binary in a pseudo-terminal |
| github.com/charmbracelet/x/vt | v0.1.0 | MIT | Terminal emulator that turns the output into a readable screen |
| github.com/charmbracelet/x/exp/ordered | v0.1.0 | MIT | Pulled in by x/vt |
