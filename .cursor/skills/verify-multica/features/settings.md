# Settings

Settings shows the signed-in person's profile first, and the workspace member list on the Members tab.

## Sub-features

- `settings-profile` opens Settings on the Profile tab.
- `settings-members` opens the Members tab and shows the member count.

## How to get to it (user POV)

- Choose `Settings` in the sidebar.
- Open `/settings` while signed in.

## Driving it with verify-multica

Preconditions:

- `doctor` printed `doctor ok`.
- No user exists yet for `settings@example.com` in this run database.

The bundled command is:

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive settings
```

That command runs these steps:

- **Sign in.** The sign-in steps with email `settings@example.com`.
- **Open settings.** `browser click --role link --name Settings --exact`. The URL contains `/settings`.
- **Profile.** A heading named `Settings` is visible. A heading named `Profile` is visible.
- **Profile proof.** `browser screenshot --path settings/profile.png`.
- **Members.** `browser click --role tab --name Members --exact`. The page shows text that starts with `Members (`.
- **Members proof.** `browser screenshot --path settings/members.png` and `browser snapshot --path settings/members.aria.txt`.

## Gotchas

- Settings is a sidebar link. The older workspace menu path is not the entry this journey uses.
- The default tab is Profile. Do not expect the Members heading until the tab click.
- The members heading includes the count, as in `Members (1)`, because sign-in creates the owner membership.
