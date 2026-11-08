# Provenance

`stometa-mode` adapts five mechanisms from pstack's `poteto-mode` skill. The
wording here is paraphrased and the preferences are Stometa's; only the
mechanisms are upstream's.

| Field | Value |
|---|---|
| Upstream repository | https://github.com/cursor/plugins (`pstack/skills/poteto-mode/SKILL.md`, `pstack/agents/poteto-agent.md`) |
| Pinned commit | `ae5650febf4320793844c12c4adaa937f417f8da` (pstack 0.15.36) |
| License | MIT |

## Adapted mechanisms

| In `SKILL.md` | Upstream source |
|---|---|
| Restate the goal and the smallest expected change on the first message | `poteto-mode` Non-negotiables, first trigger |
| Todolist copies the playbook's steps verbatim; skipped steps stay as `skip: <reason>` | `poteto-mode` Playbooks |
| Read the Principles index before multi-step work and name each applied principle with its decision | `poteto-mode` Non-negotiables and Principles |
| Every claim carries its evidence or its label: measured, inferred, guess | `poteto-mode` Writing the reply |
| Locate pstack by the parent-given path, else search the plugin cache and take the highest `plugin.json` version | `poteto-agent.md` |

## Modifications

- Scoped to Claude Code; Codex and OpenCode skip the pstack rows.
- Search path changed from `~/.cursor/plugins/` to `~/.claude/plugins/cache/`.
- Linear through `orca linear` and PRs through `gh` are Stometa's bindings, not upstream's.
- No playbook or principle text is copied; they are reached by path.
