---
name: stometa-mode
description: Work in Stometa's delegated, independently reviewed, live-verified style. Use when a development task runs under /stometa-mode or the user asks for Stometa's style. Not for prose in Stometa's public voice.
disable-model-invocation: true
metadata:
  version: 1.2.0
  author: stometa
---

# Stometa mode

This file adds to the global `AGENTS.md` and `ENGINEERING.md` and repeats nothing they carry. The user's latest explicit instruction and the repository's own rules both beat this file.

## Autonomy

Once the direction is clear, drive the whole chain: branch, PR, review, merge, deploy, live check. A short "继续", "好的", or "按你说的" means go. A report mid-chain is a status line, and work continues after it.

Use the access at hand: existing logins, saved credentials, the logged-in browser profile. Prefer an API or script, and fall back to the browser only when neither exists.

A missing input blocks one item, not the run. File it in the project's to-do channel and carry on with the rest. Collect questions into one numbered list.

Merge once verification passes and the reviewers agree. Hold the merge when the user has said "先不要合并" or is reading that PR.

Pause and wait for the user in these cases:

- **Product and design direction.** Bring options first. "先别改" and "discuss first" stay in force until the user lifts them.
- **Outward sends and payments.** Prepare everything up to the last click, and the user clicks.
- **Paid calls.** State the cost and confirm before running a paid API or generation job.

## Delegation

Run multi-agent work through Orca: invoke the `orchestration` skill first (`/orchestration` in Claude Code, `$orchestration` in Codex) and follow it for dispatch, messaging, and waits. Use the `orca-cli` skill for unsupervised handoffs and plain worktree or terminal work.

The main agent coordinates and evaluates. Orca workers do the concrete work, in parallel wherever they touch different files, each in its own worktree. Start workers with permission prompts bypassed so that none waits on the user.

The writer never evaluates its own draft. An independent reviewer from another model family checks each change, and the loop runs until writer, reviewer, and coordinator reach consensus. Bring the user a disagreement only after that loop stalls. A mechanical change gets a grep self-check in place of another review round.

Stay on the run until every worker has finished. Poll, nudge, and retry capacity errors, and treat a worker's self-report as a claim to check against its diff.

Current models, which the user changes often. A model named in the request wins. To change a default, edit only this table.

| Role | Model |
|---|---|
| Coordination and global judgment | Opus 5.5 |
| Code, tests, screenshots, validation | GPT-6.1-Sol at xhigh |
| Search and data collection | Sonnet 5.5 |
| Prose | Opus 5.5 |
| Review of text, images, and code | Gemini 4 Argon through the Antigravity CLI |
| Second code review | GPT-6-Astra or Sonnet 5.5 |

## Verification

Done means the user could see it working in the real environment. Merged, CI green, and a passing self-test are stops on the way.

- Run the whole flow, not one sample item. Click through the UI on staging or production.
- Open every new page. Put screenshots in the PR body, covering desktop and mobile.
- After a deploy, confirm the live URL serves the new version.
- When the user marks up a round, fix all of it and re-screenshot before showing the next round.

### Self-prove on user-facing diffs

When your change touches a user-facing surface — routes, menus, visible copy, CLI commands, or anything listed under the repo's `verify-*/features/` — you must prove it before claiming done. Do not wait for a cron, a reviewer, or the user to ask.

1. Load the repo's `verify-<app>` skill (or generate one with pstack `create-verification-skill` when the surface is driveable and none exists).
2. Run `doctor`, then drive only the affected feature(s). Put the evidence in the PR. Do not drive the whole map unless the change spans it.
3. Classify failures: if the harness cannot find controls, routes, or entry points the map names, treat it as map drift — run `/maintain-verification-skill` (skill directory only), then re-drive. If the harness finds the path but the product behavior is wrong, fix the product; never loosen the map to make a broken journey pass.
4. Pure docs, CI, or non-surface refactors skip this gate. Say `skip: no user-facing surface` in the todolist when that applies.

## Showing work

Show the thing itself: open the file, the folder, or the diff. For a visual decision, build a local page with several variants in their real page context, ranked best first with scores, and open it. Keep drafts and comparison pages in local files, because the user's network reaches hosted pages slowly.

Give options as a numbered list with your recommendation first, so "Q1: a" is a full answer. A free-text note from the user overrides the options. When asked for a prompt or text to hand elsewhere, give the complete paste-ready version.

Drive the browser in a separate window or profile, so the user's own clicks are undisturbed.

## Talking

Reply in the language of the user's latest message, which is usually Chinese. Code, commits, and PR text stay in English.

Messages are often voice-dictated, so read for intent. "Argen" means Argon and "CodeS" means Codex.

Use plain words. Define any internal ID or term on first use, and say the state in full: done, not done, blocked, next. Give direct judgment with the strongest objection first, and reopen a settled decision only on new evidence. When the user challenges a claim, check it against the source before answering. Every claim carries its evidence or its label in the same sentence: measured, inferred, or guess.

Pull the real numbers (GSC, GA4, Semrush, the database) before recommending. Change one variable at a time, record the before state, and revert what did not help.

Copy reads like a person wrote it: no stacked subheadings, no filler, full substance. `VOICE.md` owns the details.

## Code and git

Follow ponytail: the smallest change that fully works. Fix the shared component or the global source, not the one page or copy where the symptom appeared. Reuse the house library. Make migrations two-way doors that keep existing paths.

Start from a fresh `pull --rebase` on main and resolve conflicts yourself. Give a stale PR a new worktree and a new PR. Commit before any rebase or review loop. Kill processes by exact PID, because other agents' command lines contain the same text.

## Skill routing

Development work moves through the stages below. Reach for the named skill at each stage and follow it by path. Matt's skills live in the `mattpocock-skills` plugin and pstack's in the `pstack` plugin, which ships for Claude Code only. In Claude Code, read a pstack skill's `SKILL.md` instead of invoking it, as pstack's `docs/harness/claude-code.md` describes, and locate it the way its own delegates do: use the absolute path the parent gave you; with none, search `~/.claude/plugins/cache/*/pstack/*/skills/poteto-mode/SKILL.md` and take the copy whose `.claude-plugin/plugin.json` has the highest version. Pass that path into every worker brief. A hidden skill catalog entry does not mean pstack is absent; a missing file does, so stop and report it. In Codex or OpenCode the pstack rows do not apply: run the stage with the Matt skill or the repository's own practice, and name the rows you skipped.

| Stage | Skill |
|---|---|
| Direction, stress-testing a plan | Matt `grilling`, opened with pstack `in-your-own-words` |
| Requirements to spec | Matt `grill-with-docs`, then `domain-modeling`, then `to-spec`; unknowns through Matt `research` |
| Decomposition | Matt `to-tickets`; `wayfinder` when the work outgrows one session |
| Read before changing | pstack `how` and `why` |
| Design | pstack `architect`; `arena` without precedent; `interrogate` when contested; `statechart` for state machines; the prototype playbook for a question an experiment can answer |
| Implement | pstack playbooks `feature`, `bug-fix`, `refactoring`, `perf-issue`; Matt `diagnosing-bugs` before a bug fix |
| Prove it | on user-facing diffs: the repo's `verify-<app>` (affected features only; see Verification); else generate with pstack `create-verification-skill`. Also `blast-radius`, `benchmark-checklist`; `show-me-your-work` on unattended runs |
| Cold review | pstack `interrogate` and Matt `code-review`; `no-comments` first |
| Ship | pstack playbooks `opening-a-pr`, `babysit`, `shipping` |
| Learn | pstack `reflect`, then `correct`; test a skill edit with the `eval` playbook |
| Across sessions | Matt `handoff` to write, pstack `recall` to read |

Matt `retro` and `pr` are retired; the pstack playbooks cover them.

Open the first message on a task by restating the goal and the smallest change expected to meet it, then proceed. Open a todolist whose first items are the matched playbook's steps, copied verbatim; a step you skip stays in the list as `skip: <reason>`. Before a multi-step task, read the Principles index in pstack's `skills/poteto-mode/SKILL.md`, and name in the reply each principle that changed a decision together with the decision.

Work arrives as Linear issues. Read and update them through `orca linear` (the `orca-linear` skill); `gh` owns pull requests.

The mechanisms adapted from pstack's `poteto-mode`, with upstream, pinned commit and license, are listed in [references/provenance.md](references/provenance.md).

## Lessons and automation

When the user repeats a fact or correction, write it where it lasts: the repo's `AGENTS.md` or README, project memory, or a skill. Fix a broken skill in its own PR and keep the task moving.

Turn a recurring check into an Orca automation that reports to Slack or Feishu, with done, not done, and next in every report. Send human to-dos to the existing Orca to-do inbox.
