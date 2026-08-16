# ADR-002 — session format compatibility

**Status**: Accepted (decision D2)

## Context

Sessions are the agent's memory and audit trail. The reference agent stores
them as JSONL v3 files with a tree structure (`id`/`parentId`), append-only
semantics, and a movable leaf pointer for branching. Two options: keep the
reference format verbatim, or design a fuji-native format.

## Decision

fuji reads and writes the JSONL **v3** session format verbatim — same header,
same entry types it produces (`message`, `thinking_level_change`,
`model_change`, `compaction`, `branch_summary`, `label`, `session_info`),
same default directory encoding
(`--<cwd with '/'→'-'>--/<ts>_<uuid>.jsonl`), same migration path v1/v2→v3.

## Consequences

- **Positive**: sessions interoperate on the same projects; sessions are
  portable between tools; the parity harness can diff fuji output structurally
  (session shape, not text).
- **Positive**: no new format engineering, no format-version bootstrap cost.
- **Negative**: the v3 format carries entry types fuji does not produce
  (`custom`, `custom_message`). Per F1, fuji ignores these on read and never
  writes them.
- **Negative**: format evolution is constrained by the reference format (fuji
  cannot freely redesign).

## Alternatives

- **Fuji-native JSONL** — clean slate, but breaks interop and the parity
  harness; rejected.
- **Same format, fuji-owned version** — keeps files but forks the format
  contract; rejected as unnecessary complexity.
