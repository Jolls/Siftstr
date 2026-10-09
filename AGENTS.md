# AGENTS.md

Guidance for AI coding agents (and humans) working in this repository.

## Project

**Siftstr** is a self-hosted daily triage gate. It pulls new items from Miniflux (RSS, YouTube channels, Reddit, podcasts, GitHub releases, newsletters) and Nostr. A single Claude scheduled task summarizes them each morning; everything else runs on Siftstr's own timers. The user then sifts each item on their phone or desktop: **archive**, **promote** (come back once with a deeper summary), or **keep** (send to Karakeep, MeTube, and so on). It is not a reader.

**Status:** pre-implementation. There is no code yet.

## Read first

- [ARCHITECTURE.md](ARCHITECTURE.md) is the source of truth for the design: lifecycle, write-back matrix, API contract, data model, and open questions.
- [ROADMAP.md](ROADMAP.md) says what's in v1 and what comes later. Don't build roadmap items early, but don't design them out either.
- `idea-generation.md` holds the original raw design notes. It is gitignored (personal notes, not published), so it may not exist in your checkout. If it does, it is historical: don't edit it, and don't treat it as current where ARCHITECTURE.md disagrees.

## Invariants

Don't break any of these without updating ARCHITECTURE.md and asking the user first.

- **No LLM calls in the app.** Summaries come from Claude through the JSON API. Don't add model SDKs.
- **Never download media.** No yt-dlp. Downloads are delegated to a destination such as MeTube.
- **Source services are the system of record.** Siftstr stores only workflow state (queue, summaries, decisions, outbox, settings), and that state is deleted after 6 months.
- **All upstream side effects go through the outbox** and must be idempotent and retry-safe.
- **One Claude run per day.** The morning scheduled task is the only LLM step. Ingest, write-back, carryover, briefing/export, and cleanup are Go code on in-app timers.
- **SQLite is the source of truth.** The markdown briefing file and `/read` are renderings of it; never parse markdown back in.
- **No source-type special cases** in triage logic. Use the per-source settings and the item's media type.
- **Actioned items stay visible** with a badge, and undo stays possible. Never remove them from the DOM.
- **Actions survive a dropped connection.** Gestures, keys, and buttons all enqueue the same action into the page's queue (mirrored to `localStorage`), which sends to `POST /sync/actions` right away and retries when the connection returns. Never send an action as a direct HTMX request, and never make the UI wait on the network to record one. No service worker or installable app.
- **Undo is server-side.** The server stores each action on arrival and holds its upstream effects for the grace period. Undo cancels held effects; it never reverses a write-back that was already sent.
- **Synced actions are idempotent by client `action_id`.** Replaying a batch must be harmless.
- **Multi-user ready.** v1 has one user, but every user-owned table has `user_id`, and every query, API key, and background job is scoped to one user. Never look up a user-owned row by ID alone, and never add a global setting for something a user would configure.
- **Single static binary.** No CGO, no Node runtime, assets embedded with `embed.FS`.
- **Docker-first and platform-neutral.** One generic multi-arch image (`amd64` + `arm64`) that anyone can run with plain Docker or Compose. No StartOS-specific code, files, or assumptions in this repo; the StartOS package is a separate repo that wraps this image. Listen on plain HTTP (default `:8080`) and never assume TLS inside the container: take the `Secure` cookie flag and absolute URLs from `SIFTSTR_BASE_URL`. All persistent state lives under `/data`. Instance config comes from env vars; everything else is stored in the database. Keep `GET /healthz` working.
- **Open-source hygiene.** No personal hostnames, feeds, keys, or account details in code, fixtures, or docs.

## Planned layout

This layout is a proposal. Update this section when the real layout lands.

```
cmd/siftstr/            main: config, wiring, server start
internal/server/        routing, middleware
internal/auth/          users, login, sessions, API keys, CSRF
internal/connections/   per-user upstream credentials (encrypted at rest)
internal/api/           JSON API used by Claude scheduled tasks
internal/ui/            HTMX handlers and templates
internal/triage/        item state machine, action log, undo
internal/sync/          /sync/actions endpoint and reconciliation
internal/briefing/      builds the daily briefing; renders /read and the markdown file
internal/ingest/        miniflux/, nostr/
internal/dest/          miniflux/, karakeep/, metube/, youtube/  (Destination impls)
internal/outbox/        queue and workers
internal/store/         SQLite access and embedded migrations
web/static/             Pico.css, app.css, later htmx, gestures.js, queue.js (action queue + sender)
web/templates/          html/template files (base, login, items); embedded via web/embed.go
docs/                   API.md and other specs
Dockerfile              multi-stage, static binary, distroless/scratch runtime
compose.yaml            example deployment for plain Docker
```

## Commands

`go.mod` exists now. The Docker commands apply once the `Dockerfile` lands:

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .            # must print nothing
node --test web/test/*.test.js   # client queue and gesture tests; dev-only, also run by `go test ./web` when node is installed
docker build -t siftstr .
docker buildx build --platform linux/amd64,linux/arm64 -t siftstr .   # release builds
```

## Conventions

**Go**
- Standard library first. Add a dependency only when it clearly earns its place.
- Pass `context.Context` through every I/O path. Wrap errors with `%w`.
- Define interfaces where they're consumed. Keep packages small.
- Every external service sits behind an interface and has an `httptest` fake. Tests never hit real services.

**HTMX and frontend**
- The server renders HTML pages and fragments.
- HTMX renders pages and settings forms. Triage actions go through the action queue instead (see Invariants).
- Keep the client JS small, vanilla, and dependency-light: gestures, the action queue, and the sender.
- On `/read`, keep controls out of the readable text flow so Android reading mode and TTS ignore them.

**Database**
- Migrations are append-only and embedded. Never edit a migration that has shipped.

**Docs**
- When you change behavior that ARCHITECTURE.md describes, update ARCHITECTURE.md in the same change.
- Don't silently resolve an item in ARCHITECTURE.md's open questions. Ask the user, then record the decision.

## Working agreements

- **Memory stays in this repo, locally.** Any memory an agent keeps about this project goes in `.agent-memory/` at the repo root. It is git-ignored: never commit it, and don't store project memory anywhere else. Memory is for short-term notes. Anything lasting belongs in its proper project file (ARCHITECTURE.md, ROADMAP.md, this file, docs/), so promote it there and delete it from memory. An empty memory folder is the goal.
- **No automatic commits.** Leave changes uncommitted so the user can review the file diffs. Commit only when the user says to, for example "commit when done". That counts as approval for that request only, not for later work. The same goes for pushing.

## Issue triage

Context for the issue skills (`/evaluate-issue`, `/update-issue`, `/implement-issues`, `/done`).

- **Labels** are the GitHub defaults: `bug`, `enhancement`, `documentation`, `accessibility`, `question`, `good first issue`, `help wanted`, plus the closing labels `duplicate`, `invalid`, `wontfix`. Use `bug` or `enhancement` as the type, and add `documentation` or `accessibility` when they apply. Don't invent new labels without asking.
- **Worth doing:** judge against [ROADMAP.md](ROADMAP.md) (what's in v1 versus later) and the Invariants above. An issue that breaks an invariant needs an ARCHITECTURE.md change and the user's approval first.
- **Schema/database:** anything under `internal/store/` (migrations, queries), the data model in ARCHITECTURE.md, or the `/data` layout. Migrations are append-only.
- **Security-sensitive:** `internal/auth/`, `internal/connections/`, API key or session or CSRF handling, `/api/*` and `/sync/actions` auth, secret handling, and anything that touches per-user isolation.
- **Spans multiple packages:** a change that crosses more than one directory under `internal/`, or touches the action queue on both the Go side (`internal/sync/`, `internal/triage/`) and the client side (`web/static/`).
- **Tracker and PRs:** GitHub via `gh`. The "No automatic commits" rule still applies: `/implement-issues` may commit and open a PR only when the user has asked for that run to do so.

## Testing priorities

1. The triage state machine: every transition, the promote-once rule, and undo.
2. Actions and undo: duplicate batches, out-of-order timestamps, undo within and after the hold, and held actions surviving a server restart.
3. Outbox idempotency and retry for each destination, using fakes.
4. API contract: golden JSON for the work package, summary submission, and `/sync/actions`.
5. Per-source behavior: granularity, max depth, and carryover at the morning run.
6. The page queue in a browser: keep queuing when the connection drops, send on reconnect, survive a reload.
7. Isolation: with two users, neither can read or act on the other's items, sources, actions, connections, or API keys. Test this even though v1 has one user.

## Security

- Never log or echo secrets (upstream API keys, the Siftstr API key, session tokens).
- API key auth applies to `/api/*` only. Session and CSRF protection apply to HTML routes and `/sync/actions`.
- Instance config (`SIFTSTR_LISTEN`, `SIFTSTR_DATA_DIR`, `SIFTSTR_BASE_URL`, `SIFTSTR_SECRET_KEY`, bootstrap admin) comes from env vars; see ARCHITECTURE.md §13. Commit `.env.example`, never `.env`.
- Per-user upstream credentials live encrypted in `connections`. Never return them from any endpoint once saved.
