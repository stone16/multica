# Inbox

Inbox shows notifications for the signed-in person. A new workspace has none.

## Sub-features

- `inbox-open` opens the inbox from the sidebar.
- `inbox-empty` shows the empty list and the empty detail pane.

## How to get to it (user POV)

- Choose `Inbox` in the sidebar.
- Open `/inbox` while signed in.

## Driving it with verify-multica

Preconditions:

- `doctor` printed `doctor ok`.
- No user exists yet for `inbox@example.com` in this run database.

The bundled command is:

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive inbox
```

That command runs these steps:

- **Sign in.** The sign-in steps with email `inbox@example.com`.
- **Open inbox.** `browser click --role link --name Inbox --exact`. The URL contains `/inbox`.
- **Empty list.** A heading named `Inbox` is visible. The text `No notifications` is visible.
- **Empty detail.** The text `Your inbox is empty` is visible.
- **Proof.** `browser screenshot --path inbox/empty.png` and `browser snapshot --path inbox/empty.aria.txt`.

## Gotchas

- The inbox first paints skeletons. Wait for `No notifications`, not for a fixed sleep.
- Creating an issue does not by itself add an inbox row for the creator. This journey expects an empty inbox because the user is new.
- The sidebar badge is absent when the unread count is 0.
