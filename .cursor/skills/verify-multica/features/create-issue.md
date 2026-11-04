# Create an issue

Create issue lets a signed-in person save a title from the composer, see that issue on the board and on its detail page, and close an unsaved draft without adding a second issue.

## Sub-features

- `create-open` opens the composer from the sidebar.
- `create-save` stores the title `Release checklist` and shows it on the board.
- `create-detail` opens that issue from the board.
- `create-cancel` closes a draft titled `Discard me` without creating it.
- `create-api` reads the same titles from `GET /api/issues`.

## How to get to it (user POV)

- Sign in, then choose the sidebar icon whose tooltip is `New issue`.
- With at least one issue on the board, each column also has its own create control. This journey uses the sidebar control from the empty state.

## Driving it with verify-multica

Preconditions:

- `doctor` printed `doctor ok`.
- No user exists yet for `create-issue@example.com` in this run database.

The bundled command is:

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive create-issue
```

That command runs these steps:

- **Sign in.** The sign-in steps with email `create-issue@example.com`. The page shows `No issues yet` and `create-issue's Workspace`.
- **Empty proof.** `browser screenshot --path create-issue/empty.png`.
- **Open composer.** Hover and click `[data-slot='sidebar-header'] [data-slot='tooltip-trigger']`. The tooltip text is `New issue`. A dialog named `New Issue` is visible.
- **Enter the title.** `browser type --role textbox --name "Issue title" --value "Release checklist" --exact`.
- **Save.** `browser click --role button --name "Create Issue" --exact`. The toast text `Issue created` is visible and a link named `Release checklist` is visible.
- **Board proof.** `browser screenshot --path create-issue/board.png` and `browser snapshot --path create-issue/board.aria.txt`.
- **Open the issue.** `browser click --role link --name "Release checklist"`. The URL matches `/issues/<id>` and the page shows `Release checklist`.
- **Detail proof.** `browser screenshot --path create-issue/detail.png`.
- **Return to the list.** `browser click --role link --name Issues --exact`.
- **Cancel a draft.** Open the composer again, type `Discard me` into `Issue title`, hover the second tooltip trigger inside `[data-slot='dialog-content']`, confirm the tooltip `Close`, and click it. The dialog count is 0. The link count for `Discard me` is 0. The `Release checklist` link is still visible.
- **Mobile shot.** Set the viewport to 390 by 844 and run `browser screenshot --path create-issue/board-mobile.png`. Then restore 1280 by 800.
- **API proof.** `browser storage` prints the token and workspace id. `GET $NEXT_PUBLIC_API_URL/api/issues?limit=50` with `Authorization: Bearer <token>` and `X-Workspace-ID: <workspace id>` returns `Release checklist` and does not return `Discard me`. The body is saved as `create-issue/api-issues.json`.

## Gotchas

- The sidebar new-issue control has a tooltip, not an accessible name. Check the tooltip text before clicking.
- The title editor is a contenteditable textbox. `fill` is the wrong operation. The drive uses `type`.
- Closing the composer stores a local draft and does not create an issue. Assert the board link and the API, not the draft field.
- The default status is `todo`. The empty state hides columns until the first issue exists.
- `Create Issue` stays disabled until the title has non-space characters.
- The detail breadcrumb link back to the list is the workspace name. The sidebar link named `Issues` is the stable return path.
- Drag-and-drop on the card can swallow a pointer drag. Use a normal click on the link.
