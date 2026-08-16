# ADR-007 — `.fuji` resource directory and `--skills <dir>`

**Status**: Accepted (decision D11)

## Context

The reference agent discovers user resources from a global agent dir (settings,
skills) and project dir (settings, skills, packages). fuji drops packages and
extensions (ADR-003) but keeps the *data* resource categories: skills and
prompt templates. Users should be able to bring their own skills without
forking.

## Decision

Mirror the reference directory convention with `.fuji`:

| reference | fuji |
|----|------|
| `~/.<agent>/settings.json` | `~/.fuji/settings.json` (or `~/.fuji/fuji.toml`) |
| `.<agent>/settings.json` | `.fuji/settings.json` |
| `~/.<agent>/skills/` | `~/.fuji/skills/` |
| `.<agent>/skills/` | `.fuji/skills/` |

Skill discovery order: explicit `--skills <dir>` > project `.fuji/skills/` >
user `~/.fuji/skills/`. Skills use the standard format (frontmatter + body,
directory per skill with `SKILL.md`, or root `.md` files) and are injected
into the system prompt via `formatSkillsForPrompt` parity.

## Consequences

- **Positive**: familiar pattern for users; skills stay user-extensible
  data in a no-extension core.
- **Positive**: no trust machinery — skills are inert data parsed from
  explicit locations and never executed.
- **Negative**: skills are prompt-only; anything that needs code must be
  bundled (fork) or a config surface.

## Alternatives

- **Share the reference agent's global dir directly** — full cross-tool session
  interop but blurs the boundary between tools and couples fuji to another
  agent's global state; noted as flagged assumption F2 in `decisions.md`
  (one-line change if preferred).
