---
name: verify-multica
description: Drive the Multica web app in a disposable workspace and prove sign-in, issue creation, inbox, agents, and settings. Use when changing Multica UI or API behavior, before claiming a fix works, or when verify-multica is named.
---

# Verify Multica

Multica's primary surface is the Next.js web app. The Go API and the `multica` CLI are secondary. This skill drives the web app in a browser and checks one API readback. It does not start the agent daemon and it does not call Claude or Codex.

Run every command from the repo root. The scripts `cd` to the repo themselves, so a relative path still works.

## Launch

Preconditions on the machine are Docker with Compose, Go 1.26 or newer, Node 22, and pnpm 10.28. Postgres comes from `docker compose up -d postgres` (`pgvector/pgvector:pg17` in `docker-compose.yml`). The script creates a new database named `multica_verify_<run-id>` on that server. It does not reuse the `multica` database.

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh launch
```

Launch exports `MULTICA_VERIFY_STATE` and writes the same path to `/tmp/multica-verify-current`. Later commands read that pointer when the variable is unset.

Ready means all of the following are true:

- `go run ./cmd/migrate up` exited 0 against the run database.
- `GET /health` returns `{"status":"ok"}`.
- `GET /login` on the localhost origin returns a document that contains `Multica`.
- `public.verification_code` exists. `/health` does not check the schema, so do not treat health alone as ready.

The dev line `[DEV] Verification code for <email>: <code>` appears in the backend log only after a sign-in request. It is not a launch-ready signal.

The backend is started with `RESEND_API_KEY` removed. With no Resend client, `SendVerificationCode` prints `[DEV] Verification code for <email>: <code>` to the backend log and returns success. Do not set `APP_ENV=production` for this run. Do not type the master code `888888`.

The browser origin is `http://localhost:<frontend-port>`. Next.js dev blocks client scripts when the host is `127.0.0.1`, so the page never hydrates there. The API stays on `http://127.0.0.1:<port>`. `FRONTEND_ORIGIN` is the localhost URL so CORS allows the page.

Launch picks free backend and frontend ports. It records them in `$MULTICA_VERIFY_STATE/meta.env`. Two runs can share the Postgres container when each run has its own state directory, ports, and database. Set `MULTICA_VERIFY_RUN_ID` to `[a-z0-9]+` before launch. The pointer file tracks only the latest launch.

`pnpm install` runs only when `node_modules/playwright` is missing.

## Doctor

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh doctor
```

Doctor exits 0 only when the recorded backend and frontend pids are alive, those processes own the recorded ports when `ss` can see socket owners, `/health` and `/login` match the launch checks, `HEAD` equals the revision recorded at launch, the database name matches `multica_verify_[a-z0-9]+`, and `verification_code` exists. Any failure means do not drive this instance. Run `cleanup`, then `launch`.

## Drive

Prefer `drive <feature>` over hand-built browser steps. The feature files list the same steps that command runs.

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh drive sign-in
.cursor/skills/verify-multica/scripts/verify-multica.sh drive create-issue
.cursor/skills/verify-multica/scripts/verify-multica.sh drive inbox
.cursor/skills/verify-multica/scripts/verify-multica.sh drive agents
.cursor/skills/verify-multica/scripts/verify-multica.sh drive settings
```

`browser goto` waits until `document.body` has a React fiber. That is the signal this Next dev server has hydrated. Do not drive the page before that, or the login form submits as a normal GET and drops the email.

`drive` runs doctor, starts a headless Chromium, runs the feature, then stops that browser. Chromium is launched with `--no-sandbox` and `--disable-dev-shm-usage` so the same command works in a container. The app under test is still the local Next server and Go API.

The browser harness speaks HTTP on `127.0.0.1` at the port in `$MULTICA_VERIFY_STATE/browser.port`. Use the shell wrapper, not the port.

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh browser start
.cursor/skills/verify-multica/scripts/verify-multica.sh browser goto --path /login
.cursor/skills/verify-multica/scripts/verify-multica.sh browser click --role button --name Continue --exact
.cursor/skills/verify-multica/scripts/verify-multica.sh browser stop
```

Stable handles in this repo:

- Login title is the card text `Multica`, not a heading. The email control is the textbox named `Email`. The submit button is `Continue`.
- The code step shows `Check your email`. The one-time-code control is `[data-slot="input-otp"]`.
- Sidebar links are named `Inbox`, `My Issues`, `Issues`, `Agents`, `Runtimes`, `Skills`, and `Settings`.
- The new-issue control is the tooltip trigger in `[data-slot="sidebar-header"]`. Its tooltip text is `New issue`. The dialog is named `New Issue`. The title control is the textbox named `Issue title`. The save button is `Create Issue`.
- Empty issues copy is `No issues yet`. Empty inbox copy is `No notifications` and `Your inbox is empty`. Empty agents copy is `No agents yet` and the button `Create Agent`.

A failed browser command writes `$EVIDENCE_DIR/failure.png` and exits non-zero.

## Evidence

Default directory: `.context/verify-multica/<run-id>/` inside the repo. Override with `MULTICA_VERIFY_EVIDENCE` before launch. `.context/` is gitignored. Cleanup must not delete it.

Each drive writes a screenshot, an ARIA snapshot, and `proof.json` under its feature directory. `drive create-issue` also writes `api-issues.json`, the issue detail screenshot, and `board-mobile.png`.

Proof has to show the action and the next state. For create-issue that is the empty board, the board with `Release checklist`, the detail page with that title, and `GET /api/issues` returning that title and not `Discard me`. A toast by itself is not proof. Do not assert through a test-only endpoint. The dev log line is the local stand-in for the verification email, and the script reads it only to type the code into the real form.

## Cleanup

```bash
.cursor/skills/verify-multica/scripts/verify-multica.sh cleanup
```

Cleanup kills the browser, frontend, and backend pids recorded for this state directory, drops only `multica_verify_<run-id>`, and deletes the state directory. It does not run `docker compose down`. It does not delete the evidence directory. After cleanup, `test -f` the `proof.json` you just wrote.

## Helpers

- `.cursor/skills/verify-multica/scripts/verify-multica.sh` is the entry point. Commands are `launch`, `doctor`, `drive`, `browser`, `read-code`, and `cleanup`.
- `.cursor/skills/verify-multica/scripts/browser.mjs` is the Playwright process. `browser start` runs it. Do not invoke it except through the shell script.
- `.cursor/skills/verify-multica/scripts/check-layout.mjs` checks frontmatter, feature headings, the Claude symlinks, and the executable bit. It does not start Multica.

```bash
node .cursor/skills/verify-multica/scripts/check-layout.mjs
```

Map files in `features/` are the source for which journeys exist. Update them in the same change as a route or label change, then rerun the affected `drive` command. `/maintain-verification-skill` is the later pass that keeps the map honest.
