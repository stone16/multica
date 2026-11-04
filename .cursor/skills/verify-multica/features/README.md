# Multica verification map

This directory is the source for user-facing checks of the Multica web app. Read this index, then follow one feature file.

## Baseline preconditions

- Launch with `.cursor/skills/verify-multica/scripts/verify-multica.sh launch`.
- Doctor must print `doctor ok` before any drive.
- The run database is `multica_verify_<run-id>` and is empty until that drive signs in.
- `RESEND_API_KEY` is unset for the backend process. The code is printed in the backend log.
- Do not drive a server you did not start with `launch`.
- Do not point the run at the shared `multica` database.

## Driving conventions

- Start from the baseline above unless the feature names another state.
- Use `drive <feature>` for a full journey.
- The bullets inside a feature file are the browser commands that drive runs, in order.
- Prefer role and accessible name. The new-issue control has no accessible name, so the command uses the sidebar-header tooltip trigger and then checks the tooltip text `New issue`.
- Keep quoted names unchanged.
- A second sign-in for the same email inside 10 seconds is rejected by the API. Use the email written in the feature file, on a fresh launch, or wait 10 seconds.
- Cleanup deletes the run database and the process state. It leaves `.context/verify-multica/<run-id>/`.

## Proof and skip reporting

- Record the feature id in `proof.json`.
- Capture the screen before and after a mutation when the feature says to.
- A create-issue proof includes the issues API response, not only the toast `Issue created`.
- If a command fails, keep `failure.png` and report the command plus the unmet precondition.
- Do not mark a feature verified because a different feature rendered the same layout.

## Features

- [Sign in](./sign-in.md) covers the email code form and the empty issues page.
- [Create an issue](./create-issue.md) covers the composer, save, detail, cancel, and API readback.
- [Inbox](./inbox.md) covers the empty inbox.
- [Agents](./agents.md) covers the empty agent list.
- [Settings](./settings.md) covers profile and the members tab.
