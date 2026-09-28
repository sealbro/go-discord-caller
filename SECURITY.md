# Security Policy

## Supported versions

Only the latest release is supported. Fixes land on `main` and ship in the next
tag — see [CHANGELOG.md](CHANGELOG.md) and the
[releases page](https://github.com/sealbro/go-discord-caller/releases).

| Version | Supported |
|---------|-----------|
| Latest release | ✅ |
| Anything older | ❌ |

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Use GitHub's private reporting form:
[**Report a vulnerability**](https://github.com/sealbro/go-discord-caller/security/advisories/new).
It creates a private advisory visible only to you and the maintainer.

Please include the release or commit, what an attacker gains, and the steps to
reproduce it. Expect a first reply within a week. If the report is valid you will
be credited in the advisory and the changelog unless you ask otherwise.

## Scope

In scope — anything in this repository that could expose a bot token, leak or
inject relayed audio, bypass the caller-role check, or let a Discord user crash
or hang a self-hosted instance:

- token handling in `internal/config` and the YAML store (`STORE_PATH`)
- the role/permission filter on captured audio (`internal/manager/allow_user.go`)
- inter-guild relay codes — anything letting a guild attach to a session it was
  not given the code for
- DAVE end-to-end encryption as used by this project

Out of scope:

- vulnerabilities in Discord itself, or in upstream dependencies — report those
  upstream (`disgoorg/disgo`, `disgoorg/godave`, `libdave`). A dependency
  advisory this project has not yet picked up is fine to report as a normal issue.
- the hosted evaluation bot advertised in the README. It is for evaluation only;
  do not test against it. Run your own instance.
- anything requiring the server owner's cooperation, or requiring Administrator
  in the target guild — those users can already do worse through Discord.
- missing hardening with no attack behind it. Send it as an issue instead.

## Operator notes

Bot tokens are full account credentials. Pass them through `.env` or the
environment, never a committed file or a container image layer. The YAML store
holds channel, role and relay-code bindings — no tokens — but treat it as
sensitive: a leaked relay code lets another guild join an active session.
