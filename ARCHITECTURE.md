# Siftstr Architecture

Status: **draft, pre-implementation.** Derived from the author's raw design notes (kept locally, not published) and later design discussion. What's planned beyond v1 is in [ROADMAP.md](ROADMAP.md). Where those notes contradict themselves or this doc, this doc wins. Anything unresolved is in [Open questions](#14-open-questions). Items marked *proposed* are suggestions made while writing this doc. They are not settled decisions.

---

## 1. Purpose

Siftstr is a self-hosted **triage gate** that sits in front of an existing reading/watching workflow. Each day it collects new items from feeds, hands them to Claude for short summaries, and lets the user sift them quickly with one of three outcomes:

- **Archive**: dismiss it.
- **Promote**: bring it back once more with a deeper summary.
- **Keep**: send it on for full reading or watching later.

Siftstr is **not** a reader. Kept items go to tools that handle the actual consumption: Karakeep (bookmarks and permanent record) and MeTube (download, watch, delete).

### Non-goals (v1)

- Summarizing in-process. Claude does the summarizing, outside the app.
- Downloading media. Siftstr never runs yt-dlp.
- Being a reader or a long-term archive. Karakeep and the daily markdown log are the durable record.
- Twitter/X (no free API, and scraping is unreliable). Nostr takes its place.
- Rule-based filtering by keyword or category. v1 has per-source settings only.
- An MCP server or plugin (possible later phase).
- Audio or video transcription.
- A native mobile app. Siftstr is a web app built mobile-first.
- Anything StartOS-specific. Siftstr is a plain Docker app; the StartOS package lives in a separate repo.
- User management features: signup, invites, admin UI. The data model is multi-user from day one (§12), but v1 runs with a single user. See [ROADMAP.md](ROADMAP.md).

---

## 2. System context

```
        +------------- Claude scheduled task (Cowork): ONE run, every morning -------------+
        |  request work -> summarize (light, deep, digests) -> push summaries back          |
        +-----------------------------------+-----------------------------------------------+
                                            | HTTPS + API key
                                            v
  Miniflux ---- pull (timer) ---->  +--------------+  ---- mark read ------------> Miniflux
  Nostr relays - subscribe ------>  |   Siftstr    |  ---- bookmark + summary ---> Karakeep
                                    |  Go + HTMX   |  ---- download trigger -----> MeTube
                                    |   SQLite     |  ---- Watch Later (?) ------> YouTube
                                    +------^-------+  ---- daily .md briefing ---> Nextcloud / Obsidian
                                           | swipes sent as they happen;
                                           | queued in the page if connection drops
                               +-----------+-----------+
                               | Browser (phone/desktop)|
                               +------------------------+
```

**Ingestion paths (v1):**

| Content | Path |
|---|---|
| Blogs, news | RSS, via Miniflux |
| YouTube channels | Per-channel RSS, via Miniflux (title and link only) |
| Reddit subreddits/multis | Native `.rss` (top/hot/new, time window), via Miniflux. Self posts include the body text; link posts carry only a title and the outbound URL, so their light summary has little to go on. |
| Audio podcasts | Podcast RSS, via Miniflux (title and show notes only) |
| GitHub | Mainly releases (`releases.atom`). Other repo activity (issues, PRs, commits) works the same way through any feed URL. Via Miniflux. |
| Newsletters | Hidden or proxied RSS, via Miniflux |
| Nostr | Direct relay subscriptions. This is the only non-Miniflux source. |
| Karakeep "bookmarks" | Listed as a source in the notes, but undefined. See open questions. |

In practice, v1 has **two ingestors: Miniflux and Nostr**. Everything RSS-shaped goes through Miniflux.

---

## 3. Principles and invariants

1. **Source services own content and read state.** Siftstr writes outcomes back (Miniflux read state, Karakeep bookmarks). It never becomes the system of record for an article or video.
2. **Siftstr owns only the triage workflow.** That means the pulled-item queue, summaries, triage decisions, the write-back outbox, and its own settings. Nothing outside the settings is needed after 6 months. *(This refines the original "stateless" goal. Per-source settings, the promote-once rule, the backlog, and undo all need local state.)*
3. **One Claude run per day.** The morning scheduled task is the only thing that needs an LLM. Everything else runs on Siftstr's own timers.
4. **Claude is an external worker.** The app hands out raw items plus instructions and accepts summaries back through its API. The app contains no LLM SDK and no model calls.
5. **Deterministic work lives in the app.** Ingestion, write-back, briefing assembly, export, carryover, and cleanup are implemented in Go.
6. **SQLite is the source of truth; markdown is a rendering.** The daily briefing file and the `/read` page come from the same data and template. Siftstr never parses markdown back in.
7. **Swipes survive a dropped connection.** Every swipe or keypress goes into a small queue in the page and is sent right away. If the connection drops, actions keep queuing and are sent when it comes back. The UI never waits on the network to record an action.
8. **Failures retry.** A failed summary waits for the next morning run. A failed write-back or upload retries on the next cycle. There is no elaborate error UI. Every write-back and every synced action must be idempotent.
9. **Nothing vanishes on action.** An actioned item stays in place with a badge (archived / promoted / kept), and focus moves to the next unsifted item. Undo is one action away.
10. **Undo lives on the server.** The server stores each action as soon as it arrives but holds its upstream effects for a grace period. Undo cancels the held effects. An action is lost only if it never reached the server.
11. **No source-type special cases.** Reddit, Nostr, and podcasts are just sources. Behavior comes from per-source settings and the item's media type.
12. **Built to be open-sourced.** Destinations are configurable and pluggable. Nothing personal (hosts, feeds, keys) goes in code or fixtures.
13. **Multi-user by design, single user in v1.** Every user-owned row carries a `user_id`, every query and timer job is scoped to one user, and each user has their own upstream connections, API keys, settings, and Claude task. v1 creates one user and has no user management UI, but nothing may assume there is only one user. See §12.

---

## 4. Components

**Server (Go):**

| Component | Responsibility |
|---|---|
| HTTP server | Go `net/http`: HTML/HTMX routes, JSON API, action sync endpoint, auth middleware |
| Accounts | Users, password login, sessions, per-user API keys, per-user upstream connections (secrets encrypted at rest) |
| Scheduler | In-app timers for ingestion, releasing held actions, and the janitor. Ingestion runs once per user. |
| Miniflux ingestor | Mirrors feeds and categories into `sources`; pulls unread entries on a timer into the database; dedupes by entry ID |
| Nostr ingestor | Keeps subscriptions open to configured relays and pubkeys/topics; dedupes by event ID |
| Work dispenser | When Claude asks for work, applies carryover, then builds the work package from whatever is in the database: pending items grouped by source and mode, with resolved prompts |
| Briefing builder | Once summaries arrive, assembles the day's briefing (Today + Backlog) and writes the markdown file |
| Action log and triage service | Stores incoming actions idempotently, runs the item state machine, handles undo by cancelling held effects |
| Write-back outbox | Holds each action's upstream effects for the grace period, then sends them, with retry |
| Destination adapters | Miniflux, Karakeep, MeTube, YouTube, behind a common interface |
| Janitor | Deletes workflow data older than 6 months |

**Client (browser):**

| Component | Responsibility |
|---|---|
| Action queue | Queue of not-yet-acknowledged actions, each with a client-generated ID and timestamp. Mirrored to `localStorage` so a reload or a tab killed by the OS doesn't lose them. |
| Sender | Sends queued actions immediately. On failure, retries on the browser's `online` event, on an interval, and on the next page load. |
| Gesture/keyboard script | Turns swipes and keys into queued actions and updates the badge right away |

There is no service worker or installable app. The page must be loaded while online. After that, swiping keeps working through a dropped connection.

---

## 5. Item lifecycle

### States

| State | Meaning |
|---|---|
| `pending_light` | Ingested; waiting for a light summary at the next morning run |
| `light` | Light summary ready; shown in Today, or in Backlog if carried over |
| `pending_deep` | Promoted; waiting for a deep summary at the next morning run |
| `deep` | Deep summary ready; shown again for a final decision |
| `archived` | Terminal |
| `kept` | Terminal |
| `expired` | Dropped by carryover rules (typically digest sources) |

### Transitions

| From | Trigger | To | Notes |
|---|---|---|---|
| (new) | ingest timer | `pending_light` | Whenever the timer finds the item. It waits until Claude next asks for work. |
| `pending_*` | summary received | `light` / `deep` | |
| `pending_*` | summary failed or missing | unchanged | Retried at the next morning run |
| `light` | archive (swipe left / `Left`) | `archived` | |
| `light` | promote (swipe right / `Right`) | `pending_deep` | Allowed **once** per item. The deep summary arrives the next morning. |
| `light` | keep (tap / `Space`) | `kept` | Skips further summarization |
| `light` | no action by next morning run | `light` (Backlog) or `expired` | Depends on the per-source `carryover` setting |
| `deep` | archive | `archived` | |
| `deep` | promote **or** keep | `kept` | At this stage promote and keep do the same thing |
| `deep` | no action by next morning run | ? | Open question |
| any actioned | undo | previous state | Only while the server is still holding the action (grace period) |

The day boundary is the **morning run**: carryover is applied when Claude asks for work, so no separate evening job is needed.

The notes say "no action = sent on for a light summary next time." This doc reads that as: the item carries into Backlog with the light summary it already has, and isn't re-summarized. *To confirm.*

---

## 6. Sources and per-source settings

Sources belong to a user. They are mirrored from that user's Miniflux feed list (with categories) and from the Nostr follows they configure in Siftstr. The Settings page (`/settings/sources`) has one form per source, saved independently, with three independent settings:

| Setting | Values | Default *(proposed)* | Notes |
|---|---|---|---|
| `granularity` | `individual` / `digest` | `individual` | `digest` rolls the source's daily batch into one combined summary item (e.g. a news subreddit) |
| `max_depth` | `light` / `deep` | `deep` | Caps how deep a summary the source can support |
| `carryover` | `carry` / `drop` | `carry` for individual, `drop` for digest | What happens to untriaged items at the next morning run |
| `prompt_override` | text | none | Overrides the global summarization prompt for this source |
| `enabled` | bool | true | |

**Media type** (`article`, `video`, `podcast`, `post`) is derived per item from the URL host, feed enclosures, or the ingestor. It decides which write-back destinations apply and what the deep pass involves.

**What "deep" means:**

- **Article:** the full content from Miniflux where available, or Claude fetches the URL.
- **Video/podcast** (no transcript): visit the video or episode page, skim comments for reactions, and quickly look up the people, subjects, or terms named in the title. This is a modest step up from metadata, not exhaustive research.

**Excerpt rule (ingestion):** use the feed's summary/description field when it has one. Otherwise, truncate the full content to a configurable length. Miniflux's entry API returns only the content, not the feed's summary field, so for Miniflux items the excerpt is always the content stripped of HTML and truncated to `excerpt_length` (user setting, default 500 characters).

**Digests:** one digest item per source per day. Its underlying entries are tracked as children. What each action does to a digest is an open question; see below.

---

## 7. Actions, undo, and write-back

### From swipe to server

1. A swipe, key, or tap creates an action, e.g. `{action_id: uuid, item_id, kind: archive|promote|keep|undo, target_action_id?, at: client timestamp}`. The page adds it to its queue and immediately shows the badge.
2. The page sends it to `POST /sync/actions` right away. If that fails because the connection dropped, the action stays queued and goes out with the rest once the connection is back. An action leaves the queue only after the server confirms it.
3. The server applies actions **idempotently by `action_id`** and in client-timestamp order. It returns the current state of each affected item, and the client reconciles its badges with that.
4. An action that can no longer apply is rejected with the item's current state, and the client shows the server's version. Examples: the item has expired, or an undo whose write-back already went out.

If the session has expired, queued actions stay in the page (and `localStorage`) and are sent after the next login.

### Undo and the server-side hold

The server **stores** an action in SQLite as soon as it arrives, so the badge state is safe from then on, including across a server restart. It **holds** the action's upstream effects for a **grace period** (default 15 minutes from receipt, configurable) before acting on them.

- **Undo** is just another action (`kind: undo`, pointing at the original `action_id`). It is queued and sent the same way. If the original is still held, the server cancels its effects and the item returns to its previous state.
- **Undo after the hold ends** is refused *(proposed)*, and the client shows the item's real state. Reversing a sent write-back (e.g. deleting a Karakeep bookmark) is out of scope for v1.
- Because the hold starts when the server *receives* an action, a swipe and its undo queued together during a dropped connection arrive together, and the undo still works.

### Write-back (server to upstream)

When the hold ends, a timer releases the held effects into the **outbox**. Workers are idempotent (for example, check whether the bookmark already exists before creating it) and retry on failure. A kept video therefore reaches MeTube about 15 minutes after the swipe reaches the server, not the next day.

| Outcome | Miniflux | Karakeep | Video destinations (multi-select) | YouTube |
|---|---|---|---|---|
| archive | mark read | none | none | none |
| promote | mark read *(proposed; see open questions)* | Create bookmark, **archived**, tagged `sifted`, with the deep summary in the bookmark summary field. This waits until the deep summary exists (next morning run). | none | none |
| keep: article / post | mark read | Create bookmark with summary | n/a | n/a |
| keep: video | mark read | If Karakeep is a selected destination: link, title, and summary (no file) | e.g. MeTube download | Watch Later add *(feasibility in doubt)* |
| keep: podcast | mark read | Create bookmark with summary | ? | n/a |

Nostr items have no upstream read state, so archive is a local-only change.

MeTube is a plain URL downloader with no YouTube account behind it, so it can't do likes, history, or playlists. Anything account-related (Watch Later) has to go through the YouTube Data API, and YouTube watch history isn't available through the API at all.

**Video destinations** are a multi-select setting, e.g. Karakeep (permanent log) **and** MeTube (transient download). They are implemented behind one interface so other users can add their own:

```go
type Destination interface {
    Name() string
    Send(ctx context.Context, item Item) error // must be idempotent
}
```

**Operator requirements (document these in the README):**

- Turn off Karakeep's native Miniflux integration. Otherwise every item reaches Karakeep regardless of triage.
- Keep Karakeep's `CRAWLER_VIDEO_DOWNLOAD` off if MeTube handles downloads. It's an instance-wide setting, and Siftstr never asks Karakeep to download anything.

---

## 8. Daily cycle

### All day: Siftstr timers (no LLM)

- **Ingest timer** (e.g. every 30 minutes): keeps adding new Miniflux entries and Nostr events to the database as `pending_light`. Whatever is there when Claude asks for work gets summarized. Nothing triggers ingestion on demand.
- **Release timer** (e.g. every minute): releases actions whose hold has ended and sends their write-backs.
- **Janitor** (daily): deletes data older than 6 months.

### Morning: the one Claude scheduled task (e.g. 06:00)

Each user runs their own scheduled task with their own API key. The key identifies the user, so everything below is scoped to that user, and the day boundary is per user.

1. `POST /api/v1/runs`. Siftstr doesn't fetch anything new. It:
   - applies carryover to yesterday's untriaged items (move to Backlog or expire);
   - builds a work package from whatever is already in the database: light-summary items, digest bundles, and deep-summary items (yesterday's promotes), each with its resolved instructions;
   - sends it back.
2. Claude writes the summaries.
3. `POST /api/v1/runs/{id}/summaries`. Siftstr:
   - stores the summaries (items with no summary stay pending for tomorrow);
   - builds today's briefing (Today + Backlog);
   - writes today's markdown file;
   - queues the promote saves to Karakeep that were waiting on deep summaries.

The user then opens Siftstr and triages. If the connection drops mid-session, swipes queue in the page and are sent when it returns.

### Markdown briefing file

- It is written once the morning summaries arrive.
- It is rewritten with final outcomes (archived / promoted / kept / carried over) at the next morning run, before the new day's file starts. The finished file is the durable log of that day.
- It is generated from the same data and template as `/read`, so the file matches what the user saw.

---

## 9. Claude contract (draft)

The scheduled-task prompt should stay thin and stable. All variable instructions (the user's global prompt and per-source overrides) are **served by the app inside the work package**. The API key determines which user's work is returned. The full spec belongs in `docs/API.md` (to do). Sketch:

```jsonc
// POST /api/v1/runs  ->  201
{
  "run_id": "2026-10-09T06:00:00Z",
  "instructions": { "global": "...", "light": "...", "deep": "...", "digest": "..." },
  "items": [
    {
      "id": "itm_123", "stage": "light", "media_type": "article",
      "source": { "id": "src_9", "name": "Some Blog", "prompt_override": null },
      "title": "...", "url": "https://...", "excerpt": "...", "content": null
    }
  ],
  "digests": [
    {
      "id": "dig_7", "source": { "id": "src_4", "name": "r/news", "prompt_override": "..." },
      "entries": [ { "title": "...", "url": "...", "excerpt": "..." } ]
    }
  ]
}

// POST /api/v1/runs/{run_id}/summaries
{ "summaries": [ { "id": "itm_123", "stage": "light", "summary_md": "..." } ] }
```

The default summarization prompt ships with the app and can be edited globally and per source in Settings, in the style of a CLAUDE.md file.

**Action upload** (browser, session auth):

```jsonc
// POST /sync/actions
{ "actions": [ { "action_id": "uuid", "item_id": "itm_123", "kind": "keep", "at": "2026-10-09T07:12:03Z" } ] }
// -> 200
{ "results": [ { "action_id": "uuid", "status": "held|duplicate|rejected", "release_at": "2026-10-09T07:27:03Z", "item": { "id": "itm_123", "state": "kept" } } ] }
```

---

## 10. Data model (sketch)

```
-- accounts
users          id, username UNIQUE, password_hash, role[admin|user], timezone,
               disabled, created_at
sessions       id (hashed token), user_id, created_at, expires_at, last_seen_at
api_keys       id, user_id, key_hash, label, created_at, last_used_at, revoked_at
connections    id, user_id, kind[miniflux|karakeep|metube|youtube|nextcloud|nostr],
               base_url, secret_enc, config_json

-- workflow (every row is user-owned)
sources        id, user_id, kind[miniflux_feed|nostr], external_id, name, category,
               granularity, max_depth, carryover, prompt_override, enabled
               UNIQUE(user_id, kind, external_id)

items          id, user_id, source_id, external_id, media_type, url, title,
               excerpt, content, state, light_summary, deep_summary, promoted_at,
               batch_date, digest_id, created_at, updated_at
               UNIQUE(user_id, source_id, external_id)

digests        id, user_id, source_id, batch_date, summary, state

actions        action_id (client UUID, PK), user_id, subject_type[item|digest],
               subject_id, kind, target_action_id, client_at, received_at,
               release_at, status[held|released|cancelled|rejected]

outbox         id, user_id, action_id, connection_id, op, payload, status,
               attempts, last_error, created_at, done_at

runs           id, user_id, started_at, summaries_at, finished_at, stats

-- settings
user_settings      user_id, key, value   -- global prompt, video destinations,
                                         -- excerpt length, export path
instance_settings  key, value            -- timer intervals, grace period,
                                         -- retention (6 months)
```

The split matters: anything a user can change lives in `user_settings` or `connections`. Only operator-level knobs live in `instance_settings`.

An item's current outcome is derived from its latest effective action. The janitor deletes rows older than 6 months, measured from `created_at`. Migrations are embedded in the binary and append-only.

---

## 11. UI

| Route | Purpose |
|---|---|
| `/today` | Today's batch |
| `/backlog` | Untriaged items carried over from earlier days |
| `/read` | Today's summaries as one clean, linear page that works with Android reading mode and TTS |
| `/settings/sources` | Per-source settings table |
| `/settings/prompts` | Global and per-source summarization instructions |
| `/settings/destinations` | Video destinations, export target, upstream connections |
| `/login` | Login |

- **Mobile:** a card stack. Swipe left archives, swipe right promotes, tap keeps.
- **Desktop:** a compact list. `Up`/`Down` move between items, `Left` archives, `Right` promotes, `Space` keeps. *Proposed:* `u` undoes.
- **All actions go through the page's action queue.** Gestures, keys, and buttons all enqueue the same action shape, and no action is a direct HTMX request. HTMX still renders pages and settings forms. The badge updates as soon as the action is queued, and a small indicator appears only while actions are waiting to be sent.
- **Gestures:** a small pointer-events script detects swipes and enqueues the action. After each action, the client scrolls to the next unsifted item.
- **Reading page:** the same swipe actions work when scrolling visually. Gesture affordances must stay out of the readable text flow so TTS doesn't read them aloud. Use semantic `<article>` markup.
- **Layout** switches with CSS media queries, not user-agent sniffing.
- **Styling:** Pico.css, embedded in the binary. No CSS build step.

---

## 12. Security

### Login (v1)

- **Username and password** on a login form, which creates a server-side session. The session cookie is `HttpOnly`, `Secure`, and `SameSite=Lax`. This is simple login rather than HTTP Basic auth: Basic auth has no logout, sends the password on every request, and handles multiple users poorly.
- Passwords are stored with argon2id.
- Failed logins are rate-limited per username and per IP.
- CSRF protection applies to state-changing HTML routes and `/sync/actions`.
- **Long sessions *(proposed)*.** Make sessions long-lived so a session expiring mid-triage is rare. Queued actions survive an expired session either way.
- **Bootstrap:** on first start with an empty `users` table, Siftstr creates an admin user from `SIFTSTR_ADMIN_USER` and `SIFTSTR_ADMIN_PASSWORD`. The `siftstr user add` and `siftstr user set-password` CLI commands can also create users and reset passwords, which also gives platform wrappers (such as the StartOS package) a way to set the admin password. v1 has no other user management.
- **Behind a TLS proxy.** Inside the container every request arrives as plain HTTP, so the `Secure` cookie flag and any absolute URLs come from config (`SIFTSTR_BASE_URL`), not from `r.TLS`. Don't reject requests based on the `Host` header.

### API keys

- `Authorization: Bearer <api key>`, valid on `/api/*` only.
- Keys belong to a user, are stored hashed, and can be created and revoked from that user's Settings. A key only ever sees its owner's data.
- Keys start with `sft_` and are shown once, at creation. Until the Settings page exists, `siftstr apikey create|list|revoke <username>` manages them. Session cookies are not accepted on `/api/*`, and API keys are not accepted anywhere else.
- `GET /api/v1/ping` returns `{"user": "<username>"}` for a valid key, so a scheduled task can check its setup before a run.

### Multi-user rules (apply from v1)

- **Scoping:** every user-owned row has `user_id`. Every read and write filters by the authenticated user. A row is never looked up by ID alone. If a request names another user's item, the response is `404`, not `403`.
- **Upstream credentials are per user**, stored in `connections` and encrypted at rest with an instance key (`SIFTSTR_SECRET_KEY`, or auto-generated into `/data/secret.key` on first start; see §13). They are entered in Settings, never logged, and never returned by any endpoint once saved. Users can point at the same MeTube or Karakeep instance, but each has their own connection row.
- **Background jobs are scoped too.** Ingest, release, and export run per user, with that user's connections. One user's failing upstream must not block another user's jobs.
- **The client queue is per user.** The `localStorage` queue is keyed by user ID and only sent under a matching session. Logging out with unsent actions shows a warning.
- **Markdown export paths are per user**, e.g. a path template that includes the username.

### Exposure
The Claude scheduled task has to reach the instance over the network. That affects how it gets exposed (LAN only, VPN such as Tailscale, Tor, or a public domain). See open questions.

---

## 13. Tech stack and deployment

| Concern | Choice | Notes |
|---|---|---|
| Backend | Go | Single static binary |
| HTML | `html/template` | **Decided.** Stdlib only; no codegen step. |
| Frontend | HTMX + small vanilla JS (gestures, action queue, sender) | No SPA framework, no service worker, no Node build |
| Client storage | `localStorage` | Unsent actions only |
| CSS | Pico.css | **Decided.** No build step. |
| DB | SQLite via `modernc.org/sqlite` | **Decided.** Pure Go, no CGO |
| Assets | `embed.FS` | Static files and migrations inside the binary |
| Nostr | `github.com/nbd-wtf/go-nostr` *(proposed)* | |
| Packaging | Docker image (`linux/amd64` + `linux/arm64`) | See Deployment below |
| License | AGPL-3.0 | **Decided.** See `LICENSE`. |

### Deployment

Siftstr ships as **one generic Docker image**, run with plain Docker or Compose on any self-hosted server. Nothing in this repo depends on StartOS. The author's StartOS package lives in a **separate repo** and wraps this same image. Where a default had to be picked, it follows [Start9's packaging guidance](https://docs.start9.com/packaging/0.4.0.x/recipe-basic-service.html), but only when that guidance also makes sense for plain Docker.

**Image**
- Multi-stage build: compile with `CGO_ENABLED=0`, then copy the static binary into a minimal runtime image (`gcr.io/distroless/static` or `scratch` with CA certificates and tzdata).
- Built for `linux/amd64` and `linux/arm64`, which covers typical servers, Raspberry Pis, and StartOS hardware.
- Runs as a non-root user and needs no shell. Migrations and assets are embedded, so the image is just the binary.
- The same binary provides the CLI (`siftstr serve`, `siftstr user add`, `siftstr user set-password`).

**Networking**
- Listens on **plain HTTP**, port **8080** by default (`SIFTSTR_LISTEN=:8080`). TLS is terminated in front of it by whatever the operator uses (Caddy, Traefik, nginx, Tailscale, StartOS, etc.).
- `GET /healthz` returns `200` once the database is open and migrations have run. It serves Docker's `HEALTHCHECK` and any platform health check.

**Persistent data: one volume at `/data`**

| Path | Contents |
|---|---|
| `/data/siftstr.db` | SQLite database (WAL mode) |
| `/data/secret.key` | Instance encryption key, generated on first start if `SIFTSTR_SECRET_KEY` isn't set |
| `/data/briefings/<username>/` | Default markdown export target (can be bind-mounted to a Nextcloud/Obsidian folder) |

Everything that must survive a restart lives under `/data`, so a single named volume or bind mount is all an operator needs. Losing `secret.key` makes the saved upstream credentials unreadable (users would re-enter them), so it is backed up together with the database.

**Configuration (env vars)**

| Variable | Default | Purpose |
|---|---|---|
| `SIFTSTR_LISTEN` | `:8080` | Listen address |
| `SIFTSTR_DATA_DIR` | `/data` | Database, key, and default export location |
| `SIFTSTR_BASE_URL` | none | Public `https://` URL, used for absolute links and the `Secure` cookie flag |
| `SIFTSTR_SECRET_KEY` | auto-generated file | Encryption key for `connections` secrets |
| `SIFTSTR_ADMIN_USER` / `SIFTSTR_ADMIN_PASSWORD` | none | Bootstrap admin on first start |
| `TZ` | `UTC` | Default timezone for new users |

Everything else (timers, grace period, user settings, upstream connections) is set in the app and stored in the database, not in env vars.

**Backups**
- Back up the whole `/data` directory.
- SQLite runs in WAL mode, and copying the database file while Siftstr is writing can produce a broken copy. *Proposed:* Siftstr writes a periodic consistent snapshot with `VACUUM INTO /data/backup/siftstr.db`, so file-level backup tools (restic, borg, Nextcloud sync, StartOS) work without stopping the container.

**What the repo ships**
- A `Dockerfile` and an example `compose.yaml`: a named volume on `/data`, port 8080, and the bootstrap env vars, intended to sit behind the operator's existing reverse proxy.
- A README deployment guide written for someone who doesn't run StartOS.

**What a platform wrapper needs** (StartOS or anything else; no wrapper code lives in this repo)
- Run the image with a persistent volume on `/data`, expose port 8080 over HTTPS, and health-check `/healthz`.
- To set the admin password without env vars, run `siftstr user set-password`.

---

## 14. Open questions

1. ~~**Name:** the repo folder is `Siftsr`, but the chosen name is `Siftstr`. Which is it?~~ **Decided:** `Siftstr`. The folder is to be renamed.
2. ~~**Write-back timing.**~~ **Decided:** actions are sent as they happen (queued in the page if the connection drops). The server stores them immediately and holds their effects for 15 minutes so undo works server-side.
3. ~~**Evening job runner.**~~ **Decided:** no evening job. There is one Claude scheduled task (morning); everything else runs on Siftstr's own timers.
4. **Deep-stage items left untriaged:** carry over again, expire, or auto-keep?
5. **Digest actions:** what do archive, promote, and keep do to a digest? *Proposed:* archive marks all children read; keep saves the digest to Karakeep as a note and marks the children read; promote is disabled.
6. **Light-only sources** (`max_depth = light`): is promote disabled, or does it act as keep?
7. **Karakeep as a source:** what would be pulled from it, and how would that avoid a loop with Karakeep as a destination? Or should it be dropped as a source?
8. **YouTube Watch Later:** Google restricted API access to the Watch Later playlist years ago, so adding to it may not be possible. A user-owned playlist could be the fallback. It also needs OAuth for a self-hosted single user. **Verify before designing around it.**
9. **Karakeep summary field:** confirm the API can set a bookmark's summary and tags on create or update. Decide whether Siftstr's summary replaces or supplements Karakeep's own AI tagging and summary.
10. **Promote and Miniflux read state:** mark read when promoted, or only at the final outcome?
11. **Podcast keep destination:** Karakeep only, or something else (e.g. a podcast app)?
12. **Markdown export target:** a mounted volume that Nextcloud syncs, or a WebDAV upload? Also need the file name pattern and template.
13. **Network reachability:** where does the Cowork scheduled task run, and how does it reach a self-hosted instance (LAN, VPN, Tor, or a public domain)? The README should describe the options.
14. **Nostr configuration:** relays, followed npubs, hashtags/topics. How does the firehose case map onto `digest`?
15. **Day boundaries:** the morning run is the boundary. What timezone, and what happens if the run fails or is skipped for a day? *(Proposed: the previous briefing stays current, and the next run picks up everything.)*
16. **Janitor and Backlog:** should items still untriaged in Backlog after 6 months be deleted silently?
17. ~~**Single vs. multi-user.**~~ **Decided:** the architecture and data model are multi-user from day one. v1 runs with one bootstrapped user, and user management features are on the [roadmap](ROADMAP.md).
18. ~~**Prior art.**~~ **Decided (2026-10-09):** a web search found no project that combines Miniflux and Nostr ingest, a once-a-day Claude summary, and archive/promote/keep triage with write-back. The closest is **CondenseIt** (`wildlifechorus/condenseit`, MIT), an all-in-one digest reader: it fetches its own sources, calls an LLM itself (Ollama, OpenRouter, or an OpenAI-compatible endpoint), and ranks items from learned preferences, but has no Nostr support and no write-back. Siftstr is the integrating alternative: Miniflux and Nostr stay the system of record, decisions write back through the outbox to Karakeep, MeTube and Miniflux, and the only LLM step is one Claude run a day outside the app. Other digest tools (rssdigest, RSSbrew, RSSBox) also summarize feeds themselves. CondenseIt's source handling (Reddit thresholds, GitHub Releases, podcast search) is worth a look when those sources come up. Karakeep is a destination, not a competitor. Also considered earlier: **Readstr** (Start9 registry; upstream `privkeyio/readstr`), ruled out because it's a human-facing UI with no documented API.
19. ~~**Undo grace period.**~~ **Decided:** server-side hold, 15 minutes from receipt (configurable). *Proposed:* undo after the hold ends is refused rather than reversed upstream.
20. **Multiple devices:** actions from a phone and a desktop on the same item are ordered by client timestamp. Is last-write-wins acceptable?
21. ~~**StartOS package location.**~~ **Decided:** a separate repo. This repo stays platform-neutral.
22. **Backup snapshot:** confirm the periodic `VACUUM INTO` snapshot approach, and decide how often it runs.

---

## 15. Superseded decisions

These are kept for context. The current design is above.

| Earlier | Now |
|---|---|
| Checkbox triage with a submit button | Gestures and keys with three outcomes, queued per action |
| Four-level tap/swipe scale | Three outcomes (archive / promote / keep) |
| Promote does no write-back | Promote saves to Karakeep (archived, tagged `sifted`, deep summary) |
| One combined daily scheduled task | Briefly split into evening write-back and morning summarize jobs; now **one morning Claude task** plus in-app timers |
| Write-backs batched at an evening commit | Actions sent as they happen; the server holds each one for 15 minutes (undo window), then writes back |
| Actions sent as direct HTMX requests | Actions go through a small in-page queue that survives a dropped connection |
| Installable offline app (service worker, cached pages) | Dropped; the page only needs to keep queuing swipes after a connection drop |
| Morning request does a final ingest pass | Ingestion only happens on the timer; the request uses whatever is already in the database |
| Markdown file as the source the swipe page reads | SQLite is the source; the markdown file and `/read` page are renderings of it |
| Single video-keep destination (MeTube) | Configurable multi-select destinations |
| Actioned items disappear | Items stay with a badge and focus auto-advances |
| App is fully stateless | Source services own content state; Siftstr owns workflow state, purged after 6 months |
| Single per-source toggle | Three separate settings: granularity, max depth, carryover |
| Single user; upstream credentials in env vars | Multi-user data model; per-user connections encrypted in the database; one user in v1 |
