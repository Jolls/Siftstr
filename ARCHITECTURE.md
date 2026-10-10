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
                                    |   SQLite     |
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
| Nostr | Direct relay queries for followed npubs. This is the only non-Miniflux source. |
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
| Nostr ingestor | On the ingest timer, opens a short subscription to each of the user's relays for the followed npubs, reads until the relay signals end of stored events, and closes it. Drops events with a bad ID or signature or from an unfollowed key, skips replies, and dedupes by event ID. Items link to a public viewer (`njump.me`) because events have no URL of their own. |
| Work dispenser | When Claude asks for work, applies carryover, then builds the work package from whatever is in the database: pending items grouped by source and mode, with resolved prompts |
| Briefing builder | Once summaries arrive, assembles the day's briefing (Today + Backlog) and writes the markdown file |
| Action log and triage service | Stores incoming actions idempotently, runs the item state machine, handles undo by cancelling held effects |
| Write-back outbox | Holds each action's upstream effects for the grace period, then sends them, with retry |
| Destination adapters | Miniflux, Karakeep, MeTube, behind a common interface |
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
| `light` | promote (swipe right / `Right`) | `pending_deep` | Allowed **once** per item. The deep summary arrives the next morning. Disabled for sources with `max_depth = light` and for digests. |
| `light` | keep (tap / `Space`) | `kept` | Skips further summarization |
| `light` | no action by next morning run | `light` (Backlog) or `expired` | Depends on the per-source `carryover` setting |
| `deep` | archive | `archived` | |
| `deep` | promote **or** keep | `kept` | At this stage promote and keep do the same thing |
| `deep` | no action by next morning run | `deep` (Backlog) or `expired` | Same per-source `carryover` rule as light items; no new summary. Never auto-kept. |
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

**Age cutoff (ingestion):** entries whose published date is older than `max_age_days` (user setting, default 14, 0 = no limit) are not ingested, so connecting to a long unread backlog does not flood the first run. Entries with no date are kept. The published date is stored on the item as `published_at`.

**Digests:** one digest item per source per day. Its underlying entries are tracked as children. Archive marks all children read in Miniflux. Keep saves the digest to Karakeep as a note and marks the children read. Promote is disabled.

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
- **Last write wins.** Within a batch, actions apply in client-timestamp order. A new action replaces the subject's newest held action if its timestamp is not older; the replaced action is cancelled and the subject is restored first. An older action is rejected (`a newer action already applies`). If the earlier action was already released, the new one is rejected (`already sent upstream`). A client timestamp more than 5 minutes ahead of the server is treated as "now", so a wrong clock cannot win every comparison.
- Rejected actions are stored too, so a replay returns the same rejection. An undo is stored as already released, since it has no upstream effect. The hold is `undo_hold_minutes` in the user's settings (default 15).
- **Undo after the hold ends** is refused, and the client shows the item's real state. Reversing a sent write-back (e.g. deleting a Karakeep bookmark) is out of scope for v1.
- Because the hold starts when the server *receives* an action, a swipe and its undo queued together during a dropped connection arrive together, and the undo still works.

### Write-back (server to upstream)

When the hold ends, a timer releases the held actions, and in the same transaction their effects are queued in the **outbox** (a failure rolls both back, so an action is released only if its effects were queued). A second timer (every 30 seconds) sends due outbox entries. A kept video therefore reaches MeTube about 15 minutes after the swipe reaches the server, not the next day.

Each entry is a snapshot of what to send (URL, title, summary, tags, Miniflux entry IDs), taken when it is queued, plus the connection to send it through. Entries are de-duplicated by key (`<destination>:<action id>`), so releasing or replaying twice queues nothing new. Workers are idempotent, so an entry sent twice (a crash after the send, before the "done" mark) changes nothing upstream:

- **Miniflux:** marking an entry read twice is a no-op.
- **Karakeep:** creating a link bookmark is de-duplicated by Karakeep itself (by URL), after which Siftstr sets the wanted summary and archived flag and attaches tags; every step sets a desired state. A text note (a digest) has no URL, so it carries a `siftstr:<id>` marker in its note field and Siftstr searches for that before creating. This relies on Karakeep's search index being up to date; if it lags, a retry could create a second note.
- **MeTube:** `GET /history` is checked for the URL (queued, pending or done) before `POST /add`.

Failures retry with exponential backoff (1 minute, doubling, capped at 6 hours) up to 10 attempts, then the entry stops and keeps its `last_error`. A stop after 10 attempts is shown as **paused**, and a stop that a retry cannot fix (below) as **failed**; both can be retried or dismissed from Settings, Activity. Only a 400 or 422 reply, an item with no URL, or a connection that was removed fails at once; a rejected credential or a server that is down retries, so fixing the token in Settings lets the entry through. Error text never includes a reply body or a secret.

**Activity.** `/settings/activity` lists the user's recent outbox entries (destination, item, status queued / retrying / done / paused / failed, tries, last error) with Retry and Dismiss on stopped ones. Retry puts the entry back in the queue with a fresh attempt count and the same dedupe key, so it is still sent at most once. Dismiss hides it and sends nothing. While any stopped entry is not dismissed, every page shows an alert linking to Activity. The janitor deletes old entries with everything else (§8). Redirects are not followed, a missing secret fails at once (except MeTube's optional token), and a MeTube reply of `status: error` fails at once.

| Outcome | Miniflux | Karakeep | Video and podcast destinations (multi-select) |
|---|---|---|---|
| archive | mark read | none | none |
| promote | mark read, when the hold ends | Create bookmark, **archived**, tagged `sifted` and `promoted`, with the deep summary in the bookmark summary field. This waits until the deep summary exists (the next morning run submits it); whichever of the two happens second queues it. | none |
| keep: article / post | mark read | Create bookmark, not archived, tagged `sifted` and `kept`, with the summary | n/a |
| keep: video | mark read | If selected (default: yes): link, title, and summary (no file) | Selected destinations, e.g. MeTube download |
| keep: podcast | mark read | If selected (default: yes): bookmark with summary | Selected destinations |
| keep after promote | mark read | The same bookmark is updated: unarchived, `kept` tag added | as for keep |

The summary written is the deep summary if there is one, else the light one. Every summary Siftstr writes starts with `Agent Summary: `. Before writing, Siftstr reads the bookmark's summary: if it is empty or starts with that prefix it is replaced; if it holds other text (the user's own, or Karakeep's AI summary) that text is kept and ours is added after it, and a repeat send replaces only our part. Karakeep's own AI tagging is left alone and Siftstr only adds its tags. Digests: archive marks the children read; keep saves a text note (the digest summary) to Karakeep and marks the children read. Nostr items have no upstream read state, so archive is a local-only change and nothing is sent to Miniflux for them. A destination the user has not connected is skipped, and the action is still released.

**Which destinations a kept video or podcast goes to** is a per-user multi-select on the destinations page (`user_settings` keys `video_destinations` and `podcast_destinations`, comma-separated kinds). Unset, a video goes to every connected destination that can take one (Karakeep and MeTube) and a podcast to Karakeep. Articles and posts always go to Karakeep.

MeTube is a plain URL downloader with no YouTube account behind it, so it can't do likes, history, or playlists. YouTube account features (Watch Later, likes, history) are not in v1 (see Q8).

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
- **Janitor** (daily, per user; also once at startup): deletes the user's workflow rows older than 6 months, by `created_at`: items (including ones never triaged, see Q16), actions (`received_at`), outbox entries, runs with their `run_items` (`started_at`), digests with no remaining items (`batch_date`, since they have no `created_at`), and expired sessions. It never touches sources, connections or settings, and a second run the same day deletes nothing. Because the source entry stays unread upstream, a deleted item can be ingested again if it is still inside the source's `max_age_days` window.
- **Backup** (daily, instance-wide; also once at startup): `VACUUM INTO` writes `/data/backups/siftstr-YYYY-MM-DD.db` (UTC day) through a temp file and a rename, skips the day if the file exists, and keeps the newest 7 (Q22).

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
- It lives at `/data/briefings/<username>/YYYY-MM-DD.md` (the day is the user's local date), written atomically so a sync client never sees half a file. A failed write is logged and never fails the run or the summary submission.
- Layout: YAML frontmatter (`date`, `today`, `backlog`, `final`), then `## Today` and `## Backlog`, one `###` entry per item or digest (linked title, source and media type, outcome, summary). The summaries are Claude's markdown, inserted as is.
- Outcomes: archived, kept, promoted, expired, and at closing `carried over` for anything still waiting. Entries inside a digest are covered by the digest.
- Closing: `POST /api/v1/runs` runs carryover and then rewrites every earlier summarized day not yet closed with `final: true`. The last closed day is remembered in `user_settings` (`briefing_finalized_day`); the file is never read back.
- `/read` renders the same Today and Backlog from the same query: headings and `<article>` only, with no buttons, forms or badges in the text flow.

---

## 9. Claude contract (draft)

The scheduled-task prompt should stay thin and stable. All variable instructions (the user's global prompt and per-source overrides) are **served by the app inside the work package**. The API key determines which user's work is returned. The full spec is in [docs/API.md](docs/API.md) and the task prompt is in [docs/scheduled-task.md](docs/scheduled-task.md). Sketch:

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
{ "actions": [ { "action_id": "uuid", "subject_type": "item", "subject_id": "itm_123", "kind": "keep", "at": "2026-10-09T07:12:03Z" } ] }  // undo adds "target_action_id"
// -> 200
{ "results": [ { "action_id": "uuid", "status": "applied|duplicate|rejected", "reason": "(when rejected)", "subject_type": "item", "subject_id": "itm_123", "state": "kept" } ] }
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
               excerpt, content, published_at, state, light_summary, deep_summary, promoted_at,
               batch_date, digest_id, created_at, updated_at
               UNIQUE(user_id, source_id, external_id)

digests        id, user_id, source_id, batch_date, summary, state

actions        action_id (client UUID, PK), user_id, subject_type[item|digest],
               subject_id, kind, target_action_id, client_at, received_at,
               release_at, prev_state (state the action replaced), reason (why rejected),
               status[held|released|cancelled|rejected]

outbox         id, user_id, action_id, connection_id, op, payload, status,
               attempts, last_error, created_at, done_at,
               next_attempt_at, dedupe_key, permanent, dismissed_at
               -- UNIQUE(user_id, dedupe_key)

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
| `/settings/activity` | Outbox log: stopped updates with Retry and Dismiss |
| `/settings/destinations` | Video destinations, export target, upstream connections |
| `/login` | Login |

**Which cards a list shows.** "Today" is the user's local date (their timezone setting). `/today` shows every card summarized today, actioned or not. `/backlog` shows cards from earlier days that are still `light` or `deep`, plus earlier cards actioned recently enough to still be undone, so an action never makes a card vanish under you. A digest is one card in place of its entries. Cards still waiting for a summary, and expired ones, appear nowhere.

**Controls.** Swipe left or `Left` archives. Swipe right or `Right` promotes (on a deep card it acts as keep, and on a card that cannot be promoted it does nothing). A tap on the card body (touch only) or `Space` keeps. `u` or `z` undoes, `j`/`k` or the up/down arrows move between cards, and a key action moves focus to the next card. Keys act only on a focused card, so links, buttons and form fields keep their own keys. A touch that starts on a link or button can swipe but never counts as a tap. The page updates the badge at once and queues the action; when the server answers, its state replaces the page's guess and a rejection shows its reason on the card.

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
| Nostr | `github.com/coder/websocket` and `github.com/btcsuite/btcd/btcec/v2` (BIP-340 Schnorr) | **Decided.** A small in-repo client (bech32, event ID, signature check, one-shot REQ) instead of `go-nostr`, which pulls about 20 modules. |
| Packaging | Docker image (`linux/amd64` + `linux/arm64`) | See Deployment below |
| License | AGPL-3.0 | **Decided.** See `LICENSE`. |

### Deployment

Siftstr ships as **one generic Docker image**, run with plain Docker or Compose on any self-hosted server. Nothing in this repo depends on StartOS. The author's StartOS package lives in a **separate repo** and wraps this same image. Where a default had to be picked, it follows [Start9's packaging guidance](https://docs.start9.com/packaging/0.4.0.x/recipe-basic-service.html), but only when that guidance also makes sense for plain Docker.

**Image**
- Multi-stage build: compile with `CGO_ENABLED=0`, then copy the static binary into a minimal runtime image (`gcr.io/distroless/static` or `scratch` with CA certificates and tzdata).
- Built for `linux/amd64` and `linux/arm64`, which covers typical servers, Raspberry Pis, and StartOS hardware. CI (`.github/workflows/release.yml`) builds both with `docker buildx` and publishes to `ghcr.io/jolls/siftstr` when a `v*` tag is pushed, tagged with the version (`1.2.3`, `1.2`, and `latest` for non-prerelease tags).
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
| `/data/backups/` | Daily consistent database snapshots, newest 7 kept (no `secret.key`; back that up separately) |

Everything that must survive a restart lives under `/data`, so a single named volume or bind mount is all an operator needs. Losing `secret.key` makes the saved upstream credentials unreadable (users would re-enter them), so it is backed up together with the database.

**Configuration (env vars)**

| Variable | Default | Purpose |
|---|---|---|
| `SIFTSTR_LISTEN` | `:8080` | Listen address |
| `SIFTSTR_DATA_DIR` | `/data` | Database, key, and default export location |
| `SIFTSTR_BASE_URL` | none | URL the Claude task and browsers use to reach Siftstr: a local `http://host:port`, an IP, or a public `https://` domain. Used for absolute links; `Secure` cookies only when it is `https://` |
| `SIFTSTR_SECRET_KEY` | auto-generated file | Encryption key for `connections` secrets |
| `SIFTSTR_ADMIN_USER` / `SIFTSTR_ADMIN_PASSWORD` | none | Bootstrap admin on first start |
| `TZ` | `UTC` | Default timezone for new users |

Everything else (timers, grace period, user settings, upstream connections) is set in the app and stored in the database, not in env vars.

**Backups**
- Back up the whole `/data` directory.
- SQLite runs in WAL mode, and copying the database file while Siftstr is writing can produce a broken copy. Siftstr writes a consistent snapshot once a day with `VACUUM INTO /data/backups/siftstr-YYYY-MM-DD.db` and keeps the newest 7 (Q22), so file-level backup tools (restic, borg, Nextcloud sync, StartOS) can copy `/data/backups/` without stopping the container. Snapshots hold the encrypted connection secrets but not `secret.key`, so a restore needs the key from the original `/data/secret.key` or `SIFTSTR_SECRET_KEY`.

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
4. ~~**Deep-stage items left untriaged.**~~ **Decided (2026-10-09):** they follow the source's `carryover` rule like light items (carry to Backlog or expire). They are never auto-kept.
5. ~~**Digest actions.**~~ **Decided (2026-10-09):** archive marks all children read; keep saves the digest to Karakeep as a note and marks the children read; promote is disabled.
6. ~~**Light-only sources** (`max_depth = light`).~~ **Decided (2026-10-09):** promote is disabled for these items (the UI reads the source setting; no source-type special case).
7. **Karakeep as a source:** what would be pulled from it, and how would that avoid a loop with Karakeep as a destination? Or should it be dropped as a source?
8. ~~**YouTube Watch Later.**~~ **Decided (2026-10-09):** dropped from v1. Google closed the Watch Later playlist to API writes years ago, and a self-hosted instance would also need OAuth. A user-owned playlist is a roadmap item. There is no `youtube` destination in v1, so no YouTube column in the write-back table.
9. ~~**Karakeep summary field.**~~ **Decided (2026-10-09):** Karakeep's bookmark create and update accept a `summary` field (checked against its schema in the Karakeep source), and tags are attached with a separate call. Siftstr's summary is prefixed `Agent Summary: ` and replaces only a summary that is empty or already ours; other text is kept and ours appended (changed after the first manual test). Karakeep's AI tagging stays on and Siftstr only adds its own tags (`sifted`, plus `promoted` or `kept`).
10. ~~**Promote and Miniflux read state.**~~ **Decided (2026-10-09):** mark read when the promote's hold ends. The entry leaves the user's Miniflux unread list then, and the deep summary and Karakeep save follow later.
11. ~~**Podcast keep destination.**~~ **Decided (2026-10-09):** Karakeep by default, plus a per-user multi-select like video's so other destinations can be added later through the `Destination` interface. No second podcast adapter in v1.
12. ~~**Markdown export target.**~~ **Decided (2026-10-09):** a mounted volume only. Siftstr writes `/data/briefings/<username>/YYYY-MM-DD.md` (the day is in the user's timezone) and the operator bind-mounts that folder to whatever Nextcloud or Obsidian syncs. No WebDAV in v1. The template is fixed and built in (not user-editable): YAML frontmatter (date, counts), a Today section, then a Backlog section, one entry per item with title, source link, summary, and an outcome line once known. `/read` renders the same data with the same structure.
13. ~~**Network reachability.**~~ **Decided (2026-10-09):** the user gives Siftstr the address the scheduled task will use, in `SIFTSTR_BASE_URL`. It can be a local address with a port (`http://192.168.1.20:8080`) or an external IP or domain, and which one is the operator's call. Siftstr builds the work package's absolute URLs from it and does not check how reachable it is. The `Secure` cookie flag is set only when the URL is `https://`. The README should describe the common setups (LAN, VPN, reverse proxy, tunnel) without recommending one.
14. ~~**Nostr configuration.**~~ **Decided (2026-10-09):** one relay list per user, stored in the user's `nostr` connection and shared by every source. Sources are explicit followed npubs only (notes, kind 1, and long-form articles, kind 30023); no hashtag or topic subscriptions and no contact-list import in v1. There is no firehose special case: each npub is a source with the normal `granularity`, `max_depth` and `carryover` settings, so a busy npub is set to `digest` by the user. Nostr events carry no source-type logic in triage.
15. ~~**Day boundaries.**~~ **Decided (2026-10-09):** the timezone is a per-user setting (`users.timezone`, defaulting to `TZ` for new users). Everything stored in the database is UTC, and times are converted to the user's timezone only at the edges: when deciding which day a morning run belongs to, and when rendering. If a run fails or is skipped, nothing changes: pending items stay pending, the previous briefing stays current, and the next run summarizes everything still waiting. Carryover is applied at the start of that run, so a missed day does not expire items early.
16. ~~**Janitor and Backlog.**~~ **Decided (2026-10-09):** the janitor deletes everything older than 6 months by `created_at`, including items still untriaged in Backlog. It is silent; the entries stay unread in the source service.
17. ~~**Single vs. multi-user.**~~ **Decided:** the architecture and data model are multi-user from day one. v1 runs with one bootstrapped user, and user management features are on the [roadmap](ROADMAP.md).
18. ~~**Prior art.**~~ **Decided (2026-10-09):** a web search found no project that combines Miniflux and Nostr ingest, a once-a-day Claude summary, and archive/promote/keep triage with write-back. The closest is **CondenseIt** (`wildlifechorus/condenseit`, MIT), an all-in-one digest reader: it fetches its own sources, calls an LLM itself (Ollama, OpenRouter, or an OpenAI-compatible endpoint), and ranks items from learned preferences, but has no Nostr support and no write-back. Siftstr is the integrating alternative: Miniflux and Nostr stay the system of record, decisions write back through the outbox to Karakeep, MeTube and Miniflux, and the only LLM step is one Claude run a day outside the app. Other digest tools (rssdigest, RSSbrew, RSSBox) also summarize feeds themselves. CondenseIt's source handling (Reddit thresholds, GitHub Releases, podcast search) is worth a look when those sources come up. Karakeep is a destination, not a competitor. Also considered earlier: **Readstr** (Start9 registry; upstream `privkeyio/readstr`), ruled out because it's a human-facing UI with no documented API.
19. ~~**Undo grace period.**~~ **Decided:** server-side hold, 15 minutes from receipt (configurable). Undo after the hold ends is refused, not reversed upstream; the client shows the item's real state.
20. ~~**Multiple devices.**~~ **Decided (2026-10-09):** last-write-wins by client timestamp. A later action replaces an earlier one unless the earlier action's effects were already released to the outbox, in which case it is rejected with the item's current state.
21. ~~**StartOS package location.**~~ **Decided:** a separate repo. This repo stays platform-neutral.
22. ~~**Backup snapshot.**~~ **Decided (2026-10-09):** the janitor timer takes a `VACUUM INTO` snapshot once a day into `/data/backups/siftstr-YYYY-MM-DD.db` and keeps the newest 7. The snapshot is instance-wide (the whole database), not per user.

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
