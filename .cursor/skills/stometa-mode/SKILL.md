---
name: stometa-mode
description: Use for development work in Stometa's repositories to verify affected user journeys and report evidence.
metadata:
  version: 1.3.0
  author: stometa
---

# Stometa mode

Use the user's current instructions, repository rules, and global `AGENTS.md` and `ENGINEERING.md` as the source of truth. This skill adds a verification workflow; it does not grant permission to merge, deploy, send messages, spend money, or change production data.

## Work to completion

State the intended outcome, inspect the existing implementation, and make the smallest change that meets the request. Preserve unrelated work. Continue through implementation, applicable repository checks, and repair of failures caused by the change.

Use the tools and models available in the current runtime. Delegate when the task benefits from independent or parallel work and the runtime permits it; follow the available orchestration workflow when the user requests Orca workers. Routine changes do not require a worker or a different model family.

## Verification

### Self-prove on user-facing diffs

For a change to routes, menus, visible copy, CLI commands, or another user-facing feature:

1. Read the repository's `verify-*` skill and the affected feature description. Use its documented setup and `doctor` command when available.
2. Drive the affected journey in an isolated local or staging environment. For visual changes, inspect desktop and mobile layouts and retain screenshots. Preserve authentication, privacy, and data-loss safeguards.
3. Run the repository's applicable tests and other checks. A demonstration supplements those checks; it does not replace them.
4. Report the command, environment, observed result, and evidence location. Distinguish successful checks from unavailable or untested paths. Never report a mocked service or local fixture as production evidence.

If no verification skill exists, use the repository's established test or browser workflow. If the harness cannot find a documented route or control, inspect for map drift and repair the map only when the product behavior is correct. If the product is wrong, fix the product. Keep genuine failures visible.

For documentation, CI, or internal changes with no user-facing effect, run the relevant repository checks and state why a user journey is not applicable.

## Review and delivery

Inspect the final diff, verification results, and unresolved review findings before shipping. Use independent review when the scope or risk warrants it. Obtain authorization for external actions when the user's request does not already provide it; a brief acknowledgement does not expand the authorized scope.

After an authorized deployment, verify the live version and affected journey. Report what changed, what was verified, and any remaining gaps. Repository-specific publication and release gates remain binding.

## Maintain the workflow

Keep runnable verification commands and feature maps beside the code they exercise. Update them when the associated journey changes. Put reusable lessons in the owning repository or skill, and use the canonical source for global rules. Add automation only when requested or already authorized.
