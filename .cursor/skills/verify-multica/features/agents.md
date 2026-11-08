# Agents

Agents lists the workspace's agents. A new workspace has none and offers creation.

## Sub-features

- `agents-open` opens the agent list from the sidebar.
- `agents-empty` shows the empty copy and the create button.

## How to get to it (user POV)

- Choose `Agents` in the sidebar.
- Open `/agents` while signed in.

## Driving it with verify-multica

Preconditions:

- `doctor` printed `doctor ok`.
- No user exists yet for `agents@example.com` in this run database.

The bundled command is:

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive agents
```

That command runs these steps:

- **Sign in.** The sign-in steps with email `agents@example.com`.
- **Open agents.** `browser click --role link --name Agents --exact`. The URL contains `/agents`.
- **Empty list.** A heading named `Agents` is visible. The text `No agents yet` is visible. A button named `Create Agent` is visible.
- **Proof.** `browser screenshot --path agents/empty.png` and `browser snapshot --path agents/empty.aria.txt`.

## Gotchas

- The header plus icon has no accessible name. Use the button named `Create Agent` in the empty state.
- This journey stops before the create dialog. Opening that dialog is a different change and needs its own assertions.
- Archived agents use different copy, `No archived agents` or `No active agents`. A new workspace uses `No agents yet`.
