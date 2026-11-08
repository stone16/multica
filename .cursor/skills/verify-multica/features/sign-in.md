# Sign in

Sign in lets a person enter an email, type the 6 digit code, and land on an empty issues page for a new workspace.

## Sub-features

- `sign-in-email` submits the email form and shows the code step.
- `sign-in-code` accepts the dev log code and opens the app.
- `sign-in-workspace` shows the new workspace name and the empty issues state.

## How to get to it (user POV)

- Open `/login`.
- An unauthenticated visit to `/issues` redirects to `/login`.

## Driving it with verify-multica

Preconditions:

- `doctor` printed `doctor ok`.
- No user exists yet for `sign-in@example.com` in this run database.

The bundled command is:

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive sign-in
```

That command runs these steps:

- **Open login.** `browser goto --path /login`. The card title text is `Multica`.
- **Enter email.** `browser fill --role textbox --name Email --value sign-in@example.com --exact`.
- **Request code.** `browser click --role button --name Continue --exact`. The card title becomes `Check your email`.
- **Read the code.** `read-code --email sign-in@example.com` prints the 6 digits from the backend log.
- **Enter the code.** `browser otp --value <those digits>`. The URL matches `/issues` and the page shows `No issues yet`.
- **Workspace name.** The page shows `sign-in's Workspace`.
- **Proof.** `browser screenshot --path sign-in/issues.png` and `browser snapshot --path sign-in/issues.aria.txt`. Both show Multica's issues page, the workspace name, and `No issues yet`.

## Gotchas

- Open the frontend at `localhost`, not `127.0.0.1`. Next.js dev does not hydrate the page on `127.0.0.1`.
- The login title is a card title, not a heading. Do not require `role=heading` named `Multica`.
- The submit label is `Continue`, then `Sending code...` while the request is in flight.
- A second request for the same email inside 10 seconds returns HTTP 429 and the UI stays on the email step with an error.
- `888888` is a non-production master code. Do not use it. Type the code from the log.
- If `RESEND_API_KEY` is set, the log line is absent and this journey cannot see the code.
