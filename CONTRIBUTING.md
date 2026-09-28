# Contributing

Thanks for wanting to help 🙏 — bug reports, translations and patches are all
welcome.

- **Found a bug?** Open an [issue](https://github.com/sealbro/go-discord-caller/issues/new/choose).
  Version, raid mode and logs make it fixable; without them it usually is not.
- **Have a question or an idea?** Use
  [Discussions](https://github.com/sealbro/go-discord-caller/discussions) — it
  keeps the issue tracker to actual work.
- **Found a security problem?** Do not open an issue. See [SECURITY.md](SECURITY.md).
- **Want to translate the bot?** Skip to [Translations](#translations) — no Go
  toolchain, no libdave, no Docker needed.

## Getting the code to build

This is the part that trips people up, so read it before fighting your
toolchain: **the project needs CGO.** DAVE end-to-end encryption goes through
`disgoorg/godave`, a binding around the C++ `libdave`. A plain `go build` on a
machine without libdave will fail, and that is not a bug in your setup.

**Use Docker for development.** It is the supported path and needs nothing
installed but Docker:

```bash
docker build -t go-discord-caller .
docker run --env-file .env go-discord-caller
```

If you do want a local toolchain: Go 1.26+, plus libdave installed and
`PKG_CONFIG_PATH` pointing at its `.pc` file. Then `go build ./...` and
`go run ./cmd/bot` work as normal.

Copy `.env.example` to `.env` and fill in a `DISCORD_OWNER_BOT_TOKEN` plus at
least one `DISCORD_SPEAKER_BOT_TOKEN_1`. Both must be real Discord applications —
see the Discord app setup section of the [README](README.md#discord-app-setup).
`.env` is gitignored; keep it that way, tokens are full account credentials.

## Tests

Unit tests need no Discord bots and no network:

```bash
make test-unit          # go test --race, with coverage
go test --race ./...    # quicker, no coverage bookkeeping
```

Integration tests drive **real Discord bots** in a real guild and need libdave,
so they cannot run in Docker or in CI. They are behind a build tag and want
their own credentials in `.env.integration` — see
[docs/INTEGRATION_TESTS_SETUP.md](docs/INTEGRATION_TESTS_SETUP.md):

```bash
make test-integration                  # or: go test --tags=integration --timeout=15m ./integration/...
make test-stress STRESS_DURATION=5m    # long-running audio soak tests (tag: stress)
```

A pull request that only touches logic covered by unit tests is fine to send
with unit tests alone — say in the PR that you could not run the integration
suite and the maintainer will.

CI builds the Docker image on every pull request. It does **not** run the test
suite, so please run `make test-unit` locally before pushing.

`golangci-lint run` and `gosec ./...` are used on this project; there is no
committed config, so both run on defaults.

## Fixing a bug: write the failing test first

For anything that is a bug fix, the order matters:

1. Write a test that reproduces the broken behaviour — a unit test if the logic
   is reachable directly, otherwise an integration test.
2. Run it against the **unfixed** code and watch it fail, with a failure message
   that describes the real symptom. A test that passes before the fix proves
   nothing.
3. Then write the fix, and show the same test passing.

Keep the reproduction in its own commit if you can. Never soften an assertion to
get a test green — if the test has to change after the fix, the reproduction was
wrong, and it is worth saying so out loud.

The audio pipeline has one trap worth knowing: stopping a source to create a
lull makes the bot leave the channel, and the resulting voice-state event repairs
mixer state as a side effect — which masks any bug that needs a mixer sitting
idle while its callers stay connected. Mute the source instead. Note too that a
single caller bypasses the mixer entirely, so mixer bugs need two callers to
reproduce.

## Translations

Locales live in `internal/i18n/locales/` as one YAML file per language
(currently de, en, es, fr, pl, pt, ru, tr). To add one, copy `en.yaml` to
`<lang>.yaml` and translate the values — keep every key, keys are never
translated.

One hard constraint: **slash command and option descriptions must be 1–100
characters**, because Discord rejects the whole command registration otherwise
and the bot silently ends up with no commands. `go test ./internal/i18n/...`
checks this; run it before opening the PR.

Partial translations are fine — missing keys fall back to English.

## Pull requests

- Branch off `main`.
- Conventional-commit subject lines, imperative mood, no trailing period:
  `fix(manager): release the owner voice conn when a session auto-stops`.
- Keep the message short. A body only when the subject needs a *why*; the diff
  already shows the *what*.
- Add a `CHANGELOG.md` entry under a new `## [Unreleased]` heading for anything
  a user or operator would notice. Skip it for refactors, tests and docs.
- One logical change per PR. A drive-by refactor in a bug-fix PR makes both
  harder to review.
- Say how you tested it, and which suites you could not run.

Releases and tags are the maintainer's job — don't bump versions in a PR.

## Code style

Standard Go: `gofmt`, `go vet` clean, errors wrapped with context rather than
swallowed.

Comments are kept sparse deliberately. Prefer a clear name over a comment that
explains what the code does — a comment earns its place when it records *why*
something non-obvious is necessary, a deliberate trade-off, or a workaround for
an upstream bug. Several such comments in `internal/pool` and `internal/manager`
document real disgo bugs and the tripwire tests that will tell us when upstream
fixes them; if you touch that code, keep them accurate.

Architecture notes, should you need them: [docs/VOICE_FLOW.md](docs/VOICE_FLOW.md)
for the signal flow, [docs/LATENCY.md](docs/LATENCY.md) for the latency budget,
[docs/METRICS.md](docs/METRICS.md) for telemetry, and `CLAUDE.md` for a
package-by-package tour of the design decisions.
