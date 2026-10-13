# Siftstr Architecture

Status: **draft, pre-implementation.** Derived from the author's raw design notes (kept locally, not published) and later design discussion. What's planned beyond v1 is in [ROADMAP.md](ROADMAP.md). Where those notes contradict themselves or this doc, this doc wins. Anything unresolved is in [Open questions](#14-open-questions). Items marked *proposed* are suggestions made while writing this doc. They are not settled decisions.

---

## 1. Purpose

Siftstr is a self-hosted **triage gate** that sits in front of an existing reading/watching workflow. Each day it collects new items from feeds and lets the user sift them quickly, on the title and excerpt, with one of three outcomes. Summaries are not part of the first look: Claude summarizes digests (which have no single title to triage on) and the items the user promotes, and for sources that use Karakeep's own AI summary the promoted item shows that instead.

- **Delete**: dismiss it. Its Karakeep bookmark, if it has one, is deleted; the Miniflux entry is only marked read, never deleted. A promoted item is never deleted: on its second look the same swipe **archives** it in Karakeep instead, and the bookmark stays.
- **Promote**: bring it back once more with a summary.
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
        |  request work -> summarize (digests, promoted)    -> push summaries back          |
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
| Reddit subreddits/multis | Native `.rss` (top/hot/new, time window), via Miniflux. Self posts include the body text; link posts carry only a title and the outbound URL, so their excerpt has little to go on. |
| Audio podcasts | Podcast RSS, via Miniflux (title and show notes only) |
| GitHub | Mainly releases (`releases.atom`). Other repo activity (issues, PRs, commits) works the same way through any feed URL. Via Miniflux. |
| Newsletters | Hidden or proxied RSS, via Miniflux |
| Nostr | Direct relay queries for followed npubs. This is the only non-Miniflux source. |
| Karakeep bookmarks | Unarchived bookmarks that Karakeep itself collected (its own RSS import, or anything the user saved there), polled from Karakeep's API on the Karakeep job (§8). Already carries Karakeep's AI summary. See §6. |

In practice, v1 has **two ingestors: Miniflux and Nostr**. Everything RSS-shaped goes through Miniflux. A third, **Karakeep**, is part of the Karakeep experiment (ROADMAP.md).

---

## 3. Principles and invariants

1. **Source services own content and read state.** Siftstr writes outcomes back (Miniflux read state, Karakeep bookmarks). It never becomes the system of record for an article or video. One exception, by choice: for a source set to use Karakeep summaries (§6), the bookmark is created in Karakeep at ingest and Karakeep holds the content from then on.
2. **Siftstr owns only the triage workflow.** That means the pulled-item queue, summaries, triage decisions, the write-back outbox, and its own settings. Nothing outside the settings is needed after 6 months. *(This refines the original "stateless" goal. Per-source settings, the promote-once rule, the backlog, and undo all need local state.)*
3. **One Claude run per day.** The morning scheduled task is the only thing that needs an LLM. Everything else runs on Siftstr's own timers. Karakeep's own AI summaries are Karakeep's work, not a Siftstr LLM call.
4. **Claude is an external worker.** The app hands out raw items plus instructions and accepts summaries back through its API. The app contains no LLM SDK and no model calls.
5. **Deterministic work lives in the app.** Ingestion, write-back, briefing assembly, export, carryover, and cleanup are implemented in Go.
6. **SQLite is the source of truth; markdown is a rendering.** The daily briefing file and the `/read` page come from the same data and template. Siftstr never parses markdown back in.
7. **Swipes survive a dropped connection.** Every swipe or keypress goes into a small queue in the page and is sent right away. If the connection drops, actions keep queuing and are sent when it comes back. The UI never waits on the network to record an action.
8. **Failures retry.** A failed summary waits for the next morning run. A failed write-back or upload retries on the next cycle. There is no elaborate error UI. Every write-back and every synced action must be idempotent.
9. **Nothing vanishes on action.** An actioned item stays in place, collapsed to its title, source, a badge (deleted / archived / promoted / kept, colored per status) and Undo, and focus moves to the next unsifted item. Undo is one action away.
10. **Undo lives on the server.** The server stores each action as soon as it arrives but holds its upstream effects for a grace period. Undo cancels the held effects. An action is lost only if it never reached the server.
11. **No source-type special cases.** Reddit, Nostr, and podcasts are just sources. Behavior comes from per-source settings and the item's media type. Where a summary comes from is a per-source setting (`summary_source`), never a branch on what kind of source it is.
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
| Karakeep ingestor | On the Karakeep job, lists the user's unarchived Karakeep bookmarks newest first, 20 to a page, and creates an item for each link that is not too old, not already in Siftstr, and not one Siftstr created itself (§6). Only runs for users with a Karakeep connection. |
| Nostr ingestor | On the ingest timer, opens a short subscription to each of the user's relays for the followed npubs, reads until the relay signals end of stored events, and closes it. Drops events with a bad ID or signature or from an unfollowed key, skips replies, and dedupes by event ID. Items link to a public viewer (`njump.me`) because events have no URL of their own. |
| Work dispenser | When Claude asks for work, applies carryover, then builds the work package from whatever is in the database: digest bundles and promoted items waiting for a deep summary, with resolved prompts. Promoted items from `karakeep` sources are not included. |
| Karakeep summary poller | In the same job, right after the ingestor: reads the Karakeep bookmark of each `pending_deep` item from a `karakeep` source whose promote can no longer be undone, and stores its summary when one appears (§5, §6) |
| Highlights job | For users who turned on Karakeep highlights: caches the IDs of their Karakeep highlights and picks one a day for the briefing (§8). Read-only towards Karakeep. |
| Briefing builder | Once summaries arrive, assembles the day's briefing (Today + Backlog, with the day's highlight first under Today) and writes the markdown file |
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
| `pending_light` | A digest entry waiting for the digest's summary at the next morning run |
| `light` | First-stage card, ready to triage: title and excerpt (a digest card shows its summary). Shown in Today, or in Backlog if carried over. |
| `pending_deep` | Promoted; waiting for a summary: Claude's at the next morning run, or Karakeep's for a `karakeep` source |
| `deep` | Summary ready (or given up on); shown again for a final decision |
| `archived` | Terminal: the item was deleted (from the first look) or archived in Karakeep (from the second look, after promote). One stored state for both; see Q28. |
| `kept` | Terminal |
| `expired` | Dropped by carryover rules (typically digest sources) |

### Transitions

| From | Trigger | To | Notes |
|---|---|---|---|
| (new) | ingest timer, individual source | `light` | Whenever the timer finds the item; nothing waits on Claude. On a `karakeep` source with Karakeep connected, the bookmark is also created through the outbox (archived, no hold, since no action caused it) so Karakeep can summarize it in the background. Without a connection the source behaves as `agent`. |
| (new) | ingest timer, digest source | `pending_light` | Waits until Claude next asks for work. |
| `pending_light` | digest summary received | `light` | |
| `pending_deep` | Claude's summary received (`agent` source) | `deep` | |
| `pending_deep` | poller finds Karakeep's summary (`karakeep` source) | `deep` | Normally already there at promote, so the card returns at the next Karakeep job (every 30 minutes) after the promote's hold has ended. The card then lands on the user's current day. |
| `pending_deep` | `karakeep` source, Karakeep reports its summarization failed, its bookmark is gone, or there is no summary after `karakeep_summary_timeout_hours` (default 2) | `deep` | Shown with its excerpt and no summary, so a failed crawl or Karakeep's AI being off never strands an item. A failure is acted on at once; only a summary that is still pending waits out the timeout. |
| `pending_*` | Claude's summary failed or missing | unchanged | Retried at the next morning run |
| `light` | delete (swipe left / `Left`) | `archived` | |
| `light` | promote (swipe right / `Right`) | `pending_deep` | Allowed **once** per item. The summary arrives the next morning (`agent`) or from Karakeep within minutes (`karakeep`). Disabled for sources with `max_depth = light` and for digests. |
| `light` | keep (tap / `Space`) | `kept` | Skips the summary |
| `light` | no action by next morning run | `light` (Backlog) or `expired` | Depends on the per-source `carryover` setting |
| `deep` | archive (swipe left / `Left`) | `archived` | The bookmark stays in Karakeep, already archived and tagged `promoted`. Nothing is deleted. |
| `deep` | promote **or** keep | `kept` | At this stage promote and keep do the same thing |
| `deep` | no action by next morning run | `deep` (Backlog) or `expired` | Same per-source `carryover` rule as light items; no new summary. Never auto-kept. |
| any actioned | undo | previous state | Only while the server is still holding the action (grace period) |

The day boundary is the **morning run**: carryover is applied when Claude asks for work, so no separate evening job is needed.

The original notes say "no action = sent on for a light summary next time." With triage on titles, an untriaged item simply carries into Backlog as it is, and nothing is re-summarized.

---

## 6. Sources and per-source settings

Sources belong to a user. They are mirrored from that user's Miniflux feed list (with categories) and from the Nostr follows they configure in Siftstr. The Settings page (`/settings/sources`) has one form per source, saved independently, with three independent settings:

| Setting | Values | Default *(proposed)* | Notes |
|---|---|---|---|
| `granularity` | `individual` / `digest` | `individual` | `digest` rolls the source's daily batch into one combined summary item (e.g. a news subreddit) |
| `max_depth` | `light` / `deep` | `deep` | Caps how deep a summary the source can support; `light` disables promote |
| `carryover` | `carry` / `drop` | `carry` for individual, `drop` for digest | What happens to untriaged items at the next morning run |
| `prompt_override` | text | none | Overrides the global summarization prompt for this source |
| `summary_source` | `agent` / `karakeep` | `agent` | Where a promoted item's summary comes from, and whether the item enters Karakeep before triage. See below. |
| `enabled` | bool | true | |

**Summary source.** Every individual item is triaged on its title and excerpt, whatever its source. A summary appears only when the user promotes it (and a digest card starts as a summary, because it has no single title to triage on). `summary_source` says where that summary comes from.

`agent` (the default) keeps the item out of Karakeep until the user promotes or keeps it. Promote saves the bookmark (archived, tagged `sifted` and `promoted`), and Claude writes the summary at the next morning run, which then **overwrites** the bookmark's summary (§7). This is the right choice for feeds Karakeep can't summarize well on its own: digests (a Reddit or news roll-up is a text note, which Karakeep does not auto-summarize), Nostr events (no real page to crawl), and anything with a custom `prompt_override`.

`karakeep` has Karakeep's own AI summarize the item before the user has decided anything. At ingest, Siftstr creates the bookmark in Karakeep **archived**, so it stays out of the user's main list. The card is still triaged on title and excerpt; the summary is already there when the user promotes, with no Claude run:

- **Delete** removes the bookmark (after the normal hold, so undo works inside the grace period; once sent, a delete is not reversed, as for every write-back). This is only for items that were never promoted; on a promoted card the same swipe leaves the bookmark archived.
- **Keep** unarchives the bookmark and tags it `sifted` and `kept`.
- **Promote** leaves it archived and tags it `sifted` and `promoted`. The poller reads Karakeep's summary back and the card returns with it.

The setting requires a connected Karakeep with AI summarization on. Without a connection the source behaves as `agent`. Digest sources can't use `karakeep`, since there is no page to summarize. This path means Karakeep crawls and stores every item from the feeds that use it, including ones that end up archived, so it is meant for feeds the user chooses to try it on. The simpler alternative, for feeds the user doesn't need Siftstr to triage, is to add the feed to Karakeep's own RSS import and leave it out of Siftstr.

**Karakeep as a source.** Karakeep can collect items on its own: its RSS import, a browser extension, a share from a phone. The user sifts those in Siftstr too. On the Karakeep job (every 30 minutes) Siftstr lists the user's **unarchived** bookmarks, newest first, 20 to a page, and stops at the first one older than `max_age_days` (the same cutoff as Miniflux, measured from the bookmark's creation time), so connecting to a large library cannot flood Today. A bookmark becomes an item only if it passes all of these checks:

- **Not too old.** See above.
- **A link.** Text notes and uploaded files have no page to triage and are skipped.
- **Named by where it came from.** The card, the briefing and `/read` show the bookmark's publisher as Karakeep crawled it, or failing that the site of its link (the host without `www.`), instead of the source name "Karakeep" (`items.byline`). An item ingested before bylines existed gets its byline the next time its bookmark is listed. A byline that is already set is never changed.
- **Not already known.** No item for this user has the bookmark's ID (`external_id` on a Karakeep source, or `karakeep_id` on any item) or the same normalized URL (scheme and host in lower case, no fragment, no `utm_` or click-id parameters, no trailing slash). The URL check also covers the short window in which a bookmark Siftstr just created for a keep exists but is not tagged yet.
- **Not one Siftstr wrote.** Anything tagged `sifted` is skipped. This is what breaks the loop with Karakeep as a destination: a kept item is unarchived in Karakeep, so without the tag the next poll would offer it for triage again.

The walk reads 20 a page and stops at the first page with nothing to ingest or name, once an earlier walk has already gone back as far as the current age limit allows (`karakeep_walked_back_to`), so a steady tick reads one page however large the library; raising or removing the age limit makes the next walk go further back.

These items are treated as a `karakeep` source (`summary_source = karakeep`, no setting needed): Karakeep's own AI summary is shown on promote, and Claude is not involved. There is no Miniflux entry, so nothing is sent to Miniflux, as for Nostr. Archive deletes the bookmark, keep tags it `sifted` and `kept` and leaves it unarchived, and promote tags it `sifted` and `promoted` and archives it, the same as for any `karakeep` source. A source row of kind `karakeep` carries the per-source settings, so `carryover` and `max_depth` still apply.

**Media type** (`article`, `video`, `podcast`, `post`) is derived per item from the URL host, feed enclosures, or the ingestor. It decides which write-back destinations apply and what the deep pass involves.

**What "deep" means** on the `agent` path (on the `karakeep` path the summary is Karakeep's AI summary of the page it crawled):

- **Article:** the full content from Miniflux where available, or Claude fetches the URL.
- **Video/podcast** (no transcript): visit the video or episode page, skim comments for reactions, and quickly look up the people, subjects, or terms named in the title. This is a modest step up from metadata, not exhaustive research.

**Excerpt rule (ingestion):** use the feed's summary/description field when it has one. Otherwise, truncate the full content to a configurable length. Miniflux's entry API returns only the content, not the feed's summary field, so for Miniflux items the excerpt is always the content stripped of HTML and truncated to `excerpt_length` (user setting, default 500 characters).

**Age cutoff (ingestion):** entries whose published date is older than `max_age_days` (user setting, default 14, 0 = no limit) are not ingested, so connecting to a long unread backlog does not flood the first run. Entries with no date are kept. The published date is stored on the item as `published_at`.

**Digests:** one digest item per source per day, which starts as Claude's summary of its entries. Its underlying entries are tracked as children. Delete marks all children read in Miniflux. Keep saves the digest to Karakeep as a note and marks the children read. Promote is disabled.

---

## 7. Actions, undo, and write-back

### From swipe to server

1. A swipe, key, or tap creates an action, e.g. `{action_id: uuid, item_id, kind: archive|promote|keep|undo (`archive` is the stored name of Delete), target_action_id?, at: client timestamp}`. The page adds it to its queue, immediately shows the badge, and collapses the card.
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
- **Karakeep:** creating a link bookmark is de-duplicated by Karakeep itself (by URL), after which Siftstr sets the wanted archived flag and attaches tags; every step sets a desired state. The summary is a separate, later step (below). A text note (a digest) has no URL, so it carries a `siftstr:<id>` marker in its note field and Siftstr searches for that before creating. This relies on Karakeep's search index being up to date; if it lags, a retry could create a second note.
- **MeTube:** `GET /history` is checked for the URL (queued, pending or done) before `POST /add`.

Failures retry with exponential backoff (1 minute, doubling, capped at 6 hours) up to 10 attempts, then the entry stops and keeps its `last_error`. A stop after 10 attempts is shown as **paused**, and a stop that a retry cannot fix (below) as **failed**; both can be retried or dismissed from Settings, Activity. Only a 400 or 422 reply, an item with no URL, or a connection that was removed fails at once; a rejected credential or a server that is down retries, so fixing the token in Settings lets the entry through. Error text never includes a reply body or a secret.

**Activity.** `/settings/activity` lists the user's recent outbox entries (destination, item, status queued / retrying / done / paused / failed, tries, last error) with Retry and Dismiss on stopped ones. Retry puts the entry back in the queue with a fresh attempt count and the same dedupe key, so it is still sent at most once. Dismiss hides it and sends nothing. While any stopped entry is not dismissed, every page shows an alert linking to Activity. The janitor deletes old entries with everything else (§8). Redirects are not followed, a missing secret fails at once (except MeTube's optional token), and a MeTube reply of `status: error` fails at once.

| Outcome | Miniflux | Karakeep | Video and podcast destinations (multi-select) |
|---|---|---|---|
| delete | mark read | Delete the bookmark, for `karakeep` sources and Karakeep-sourced items. Nothing for `agent` items, which were never saved. | none |
| archive (after promote) | already marked read | None: the bookmark stays archived, tagged `sifted` and `promoted`, with its summary | none |
| promote | mark read, when the hold ends | `agent` source: create bookmark, **archived**, tagged `sifted` and `promoted`; Claude's summary then overwrites its summary field (delayed, see below). `karakeep` source: the bookmark already exists, so it only gets the tags, and Karakeep's own summary stands. | none |
| keep: article / post | mark read | Create bookmark, not archived, tagged `sifted` and `kept` (a `karakeep` source's bookmark is unarchived and tagged instead) | n/a |
| keep: video | mark read | If selected (default: yes): link and title (no file) | Selected destinations, e.g. MeTube download |
| keep: podcast | mark read | If selected (default: yes): bookmark | Selected destinations |
| keep after promote | mark read | The same bookmark is updated: unarchived, `kept` tag added | as for keep |

**Summaries written to Karakeep.** Karakeep's auto-summarize replaces whatever summary a link bookmark has when its crawl finishes, so a summary written right after creating the bookmark would be lost. Siftstr writes a summary of its own in two cases. A **digest** is a text note, which Karakeep doesn't auto-summarize, so its summary is written with the note and with no delay. A **promote on an `agent` source** writes Claude's summary in a separate outbox entry that is not due until `karakeep_summary_delay_minutes` (a user setting, *proposed* default 30) after the bookmark was created. That entry **overwrites** the bookmark's summary; it does not merge, and it sets a desired state, so a repeat send changes nothing. A keep never writes a summary, so Karakeep's own stands, and a `karakeep` source never gets one from Siftstr. Karakeep's own AI tagging is left alone and Siftstr only adds its tags. Digests: archive marks the children read; keep saves a text note (the digest summary) to Karakeep and marks the children read. Nostr items and Karakeep-sourced items have no Miniflux entry, so nothing is sent to Miniflux for them (for Nostr, archive is a local-only change). A destination the user has not connected is skipped, and the action is still released.

**What was checked against a real Karakeep (2026-10-11).** These facts shaped the design above. They were read from one instance with AI summarization on, so re-check them after a Karakeep upgrade.

- A bookmark created archived (`archived: true`) is still crawled, summarized and tagged by Karakeep's AI, about 30 to 40 seconds after creation for a short page. Archiving does not keep it out of its pipeline (Q24).
- Creating a link whose URL already exists returns the existing bookmark (`alreadyExists`) **and sets its archived flag to what the request says, or to unarchived when the request has none.** Every create therefore sends `archived` explicitly.
- `PATCH /bookmarks/{id}` takes `archived` and `summary` independently; a field left out is untouched. `POST` and `DELETE /bookmarks/{id}/tags` attach and detach tags by name (`{"tags":[{"tagName":"..."}]}`); detaching leaves the tag itself behind. `DELETE /bookmarks/{id}` returns 204.
- `GET /bookmarks?archived=false` returns the newest first (by `createdAt`), with `limit` and an opaque `nextCursor` (null on the last page). Each bookmark carries `summary`, `summarizationStatus` (`pending`, `success` or `failure`), `taggingStatus` and its tags with who attached them. Karakeep's AI adds its own tags, so the `sifted` check looks at tag names only. A page of 100 was too large to transfer reliably; pages of 20 are used. Never list all tags: the endpoint is huge.
- A library may hold far more archived than unarchived bookmarks (the one checked had about 17,000 archived and 300 unarchived). The ingestor only lists unarchived ones and stops at the age cutoff, so the archived pile is never read.

**How the outbox copes with the bookmark not existing yet.** An entry that works on an item's bookmark (`update`, `delete`, `summary`) finds the bookmark ID in `items.karakeep_id` when it is *sent*, not when it is queued, because the entry that makes the bookmark may not have gone out yet. A save records the ID it gets back. If there is still none, an `update` makes the bookmark in the wanted state from the item's URL, a `delete` has nothing to do, and a `summary` retries. The bookmark made at ingest is dropped if the item has moved past `light` by the time it is sent, so a late ingest entry can never re-archive what a keep has just unarchived. It is also made only if Karakeep has no bookmark for that URL (`GET /bookmarks/check-url`): one the user already has is theirs, so it is left as it is, not archived, and its ID is not recorded, so a later delete cannot remove it. A promote or keep is the user's own act, so those do take over an existing bookmark for the URL. An entry that has to wait for another (a summary whose bookmark is not made yet) is put back for five minutes without using up its retries. A kept video or podcast whose destinations do not include Karakeep has the bookmark Siftstr made deleted again, unless the bookmark was one the user saved in Karakeep themselves, which stays, unarchived and tagged.

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
- Turn Karakeep's AI summarization on for any user whose sources use `summary_source = karakeep`. Siftstr can't see whether it is on; without it those items wait out the timeout and show only their excerpt.

---

## 8. Daily cycle

### All day: Siftstr timers (no LLM)

- **Ingest timers** (every 30 minutes, one per ingestor): keep adding new Miniflux entries and Nostr events to the database as `light`, ready to triage at once, on the user's local day they were ingested (digest entries as `pending_light`, summarized at the next morning run). A source set to use Karakeep summaries also gets its archived Karakeep bookmark queued in the outbox at this point. Nothing triggers ingestion on demand.
- **Release timer** (e.g. every minute): releases actions whose hold has ended and sends their write-backs.
- **Karakeep job** (every 30 minutes, per user, skipped for users with no Karakeep connection): first lists unarchived Karakeep bookmarks and offers new ones as `light` items (§6), then, for each `pending_deep` item from a `karakeep` source, reads the bookmark's summary and moves the item to `deep` when it appears, or when Karakeep reports failure or the timeout passes (§5). One job rather than two timers, because a 30-minute wait for a promoted card is acceptable (the summary is normally there already) and it keeps one pass over one connection. An item whose promote is still in its undo hold is skipped, so an undo is never refused because the summary happened to arrive.
- **Highlights job** (hourly, per user; also once at startup; does nothing unless the user turned on `karakeep_highlights` and has a Karakeep connection). Karakeep has no count or random pick for highlights and its IDs are random strings, so this job caches them and picks from the cache. It has two parts:
  - *Refresh*, on the first run after a start and then weekly while Siftstr keeps running: walks `GET /api/v1/highlights` in pages of 100 (capped at 100 pages) and stores every highlight that has text in `highlight_cache`. The whole list is read before anything is written, so a failed walk changes nothing. A walk that reached the end also removes cached highlights Karakeep no longer has; one stopped by the page cap only adds. If the walk fails, the pick below still uses what is stored.
  - *Pick*, once per user-local day: if `highlight_picks` has no row for today, chooses a random cached highlight that has not been shown yet (when all have been shown, the record is cleared and they start again), reads it and its bookmark from Karakeep, and stores the text, note, color, bookmark title, link and byline with the day. A highlight or bookmark Karakeep no longer has is dropped from the cache and another is tried (up to 5). If the bookmark cannot be read the highlight is stored without a title or link; if the highlight cannot be read the day has no highlight and the next run tries again.
  The pick is made here, never while a page loads, and is stored so that rewriting the day's file, or the closing pass, shows the same highlight. It is not an item: it has no state and nothing can be done with it. The ID cache and the picks are workflow state; picks are deleted with the rest after 6 months.
- **Janitor** (daily, per user; also once at startup): deletes the user's workflow rows older than 6 months, by `created_at`: items (including ones never triaged, see Q16), actions (`received_at`), outbox entries, runs with their `run_items` (`started_at`), highlight picks (`day`), digests with no remaining items (`batch_date`, since they have no `created_at`), and expired sessions. It never touches sources, connections or settings, and a second run the same day deletes nothing. Because the source entry stays unread upstream, a deleted item can be ingested again if it is still inside the source's `max_age_days` window.
- **Backup** (daily, instance-wide; also once at startup): `VACUUM INTO` writes `/data/backups/siftstr-YYYY-MM-DD.db` (UTC day) through a temp file and a rename, skips the day if the file exists, and keeps the newest 7 (Q22).

### Morning: the one Claude scheduled task (e.g. 06:00)

Each user runs their own scheduled task with their own API key. The key identifies the user, so everything below is scoped to that user, and the day boundary is per user.

1. `POST /api/v1/runs`. Siftstr doesn't fetch anything new. It:
   - applies carryover to yesterday's untriaged items (move to Backlog or expire);
   - builds a work package from whatever is already in the database: digest bundles and deep-summary items (promotes from `agent` sources), each with its resolved instructions;
   - sends it back.
2. Claude writes the summaries.
3. `POST /api/v1/runs/{id}/summaries`. Siftstr:
   - stores the summaries (items with no summary stay pending for tomorrow);
   - builds today's briefing (Today + Backlog);
   - writes today's markdown file;
   - queues the delayed Karakeep summary overwrites for the deep summaries it received.

The user then opens Siftstr and triages. If the connection drops mid-session, swipes queue in the page and are sent when it returns.

### Markdown briefing file

- It is written once the morning summaries arrive.
- It is rewritten with final outcomes (deleted / archived / promoted / kept / carried over) at the next morning run, before the new day's file starts. The finished file is the durable log of that day.
- It is generated from the same data and template as `/read`, so the file matches what the user saw.
- It lives at `/data/briefings/<username>/YYYY-MM-DD.md` (the day is the user's local date), written atomically so a sync client never sees half a file. A failed write is logged and never fails the run or the summary submission.
- Layout: YAML frontmatter (`date`, `today`, `backlog`, `final`), then `## Today` and `## Backlog`, one `###` entry per item or digest (linked title, source and media type, outcome, then the summary, or the excerpt as a quote when there is no summary yet). A digest always has a summary; a promoted item gets its summary at the next rewrite, not live (Q25). The summaries are Claude's or Karakeep's markdown, inserted as is.
- Highlight: when the user has one for the day (§8), it is the first entry under `## Today`, as a `### Highlight` entry with the passage as a quote, the note if there is one, and a `From` line naming and linking the bookmark. It is not counted in `today:`. `/read` shows it first in Today the same way, as an `<article>` with no controls.
- Outcomes: deleted, archived (deleted after a promote), kept, promoted, expired, and at closing `carried over` for anything still waiting. Entries inside a digest are covered by the digest.
- Closing: `POST /api/v1/runs` runs carryover and then rewrites every earlier summarized day not yet closed with `final: true`. The last closed day is remembered in `user_settings` (`briefing_finalized_day`); the file is never read back.
- `/read` renders the same Today and Backlog from the same query: headings and `<article>` only, with no buttons, forms or badges in the text flow.

---

## 9. Claude contract (draft)

The scheduled-task prompt should stay thin and stable. All variable instructions (the user's global prompt and per-source overrides) are **served by the app inside the work package**. The API key determines which user's work is returned. The full spec is in [docs/API.md](docs/API.md) and the task prompt is in [docs/scheduled-task.md](docs/scheduled-task.md). Sketch:

```jsonc
// POST /api/v1/runs  ->  201
{
  "run_id": "2026-10-09T06:00:00Z",
  "instructions": { "global": "...", "deep": "...", "digest": "..." },
  "items": [
    {
      "id": "itm_123", "stage": "deep", "media_type": "article",
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
{ "summaries": [ { "id": "itm_123", "stage": "deep", "summary_md": "..." } ] }
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
sources        id, user_id, kind[miniflux_feed|nostr|karakeep], external_id, name, category,
               granularity, max_depth, carryover, summary_source[agent|karakeep],
               prompt_override, enabled
               UNIQUE(user_id, kind, external_id)

items          id, user_id, source_id, external_id, media_type, url, title,
               excerpt, content, published_at, state, light_summary, deep_summary, promoted_at,
               batch_date, digest_id, karakeep_id (bookmark id, set at ingest on the
               karakeep path), byline (where it came from, when more specific
               than the source: a Karakeep bookmark's publisher or site),
               created_at, updated_at
               UNIQUE(user_id, source_id, external_id)

digests        id, user_id, source_id, batch_date, summary, state

actions        action_id (client UUID, PK), user_id, subject_type[item|digest],
               subject_id, kind, target_action_id, client_at, received_at,
               release_at, prev_state (state the action replaced), reason (why rejected),
               status[held|released|cancelled|rejected]

outbox         id, user_id, action_id (null for ingest-driven entries), connection_id, op, payload, status,
               attempts, last_error, created_at, done_at,
               next_attempt_at, dedupe_key, permanent, dismissed_at
               -- UNIQUE(user_id, dedupe_key)

runs           id, user_id, started_at, summaries_at, finished_at, stats

highlight_cache  user_id, highlight_id, bookmark_id, shown_on   -- PK(user_id, highlight_id)
                 -- IDs of the user's Karakeep highlights that have text;
                 -- shown_on = the local day it was last picked
highlight_picks  user_id, day, highlight_id, text, note, color, bookmark_title,
                 bookmark_url, byline, created_at               -- PK(user_id, day)
                 -- the day's highlight, copied so a rewritten briefing reads the same

-- settings
user_settings      user_id, key, value   -- global prompt, video destinations,
                                         -- phone_flow, theme (auto|light|dark),
                                         -- accent (jade by default, or one of the offered Pico color families),
                                         -- excerpt length, export path,
                                         -- karakeep_summary_delay_minutes,
                                         -- karakeep_summary_timeout_hours,
                                         -- karakeep_highlights (1 = on; off by default)
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
| `/settings/appearance` | List direction on a phone, theme (follow the device, light or dark) and accent color |
| `/settings/prompts` | Global and per-source summarization instructions |
| `/settings/activity` | Outbox log: stopped updates with Retry and Dismiss |
| `/settings/sessions` | Active sessions and Sign out everywhere |
| `/settings/destinations` | Video destinations, export target, upstream connections |
| `/login` | Login |

**Which cards a list shows.** "Today" is the user's local date (their timezone setting). `/today` shows every card ingested today (a digest: summarized today), actioned or not. `/backlog` shows cards from earlier days that are still `light` or `deep`, plus earlier cards actioned recently enough to still be undone, so an action never makes a card vanish under you. A digest is one card in place of its entries. Digests still waiting for their summary, and expired cards, appear nowhere.

**Controls.** Swipe left or `Left` deletes (on a promoted card it archives: the Karakeep bookmark stays). Swipe right or `Right` promotes (on a deep card it acts as keep, and on a card that cannot be promoted it does nothing). A tap on the card body (touch only) or `Space` keeps. `u` or `z` undoes, `j`/`k` or the up/down arrows move between cards, and a key action moves focus to the next card. An actioned card is collapsed by CSS from its state: the summary or excerpt and every action button except Undo are hidden (still in the DOM), the card stays dimmed, the badge sits on the left and Undo on the right. The badge is colored by its word (`data-badge`: deleted red, archived grey, promoted light green, kept light blue) with fixed light and dark values that do not follow the accent; the label stays, and the badge is not dimmed so its text keeps AA contrast. The card takes no gestures or keys until it is undone. Undo brings the full card back. Keys act only on a focused card, so links, buttons and form fields keep their own keys. A touch that starts on a link or button can swipe but never counts as a tap. The page updates the badge at once and queues the action; when the server answers, its state replaces the page's guess and a rejection shows its reason on the card.

- **Mobile:** a card stack, drawn bottom-up. The list is reversed with CSS (`column-reverse`; the DOM keeps the sifting order), so the first card to sift sits at the bottom, under the thumb, and each next card comes down from above. The heading stays pinned at the top with a count of cards left (and a fade under it), so the cards above the one in view are not a surprise. A per-user setting (`phone_flow` in `user_settings`, `up` by default, on the Appearance settings page) switches it back to a plain top-down list; the list carries it as `data-flow`. The page opens on the first card still to sift. Swipe left deletes (archives, on a promoted card), swipe right promotes, tap keeps.
- **Desktop:** a compact list. `Up`/`Down` move between items, `Left` deletes (archives, on a promoted card), `Right` promotes, `Space` keeps. *Proposed:* `u` undoes.
- **All actions go through the page's action queue.** Gestures, keys, and buttons all enqueue the same action shape, and no action is a direct HTMX request. HTMX still renders pages and settings forms. The badge updates and the card collapses as soon as the action is queued, and a small indicator appears only while actions are waiting to be sent.
- **Gestures:** a small pointer-events script detects swipes and enqueues the action. After each action, the client scrolls to the next unsifted item (on mobile that is the card above, settling at the bottom of the screen). `j`/`k` always mean next and previous in sifting order; the arrows follow the screen.
- **Reading page:** the same swipe actions work when scrolling visually. Gesture affordances must stay out of the readable text flow so TTS doesn't read them aloud. Use semantic `<article>` markup.
- **Layout** switches with CSS media queries, not user-agent sniffing.
- **Styling:** Pico.css (the jade color build, v2.1.1), embedded in the binary. No CSS build step. Light and dark come from Pico's own themes: by default the page follows the device (`prefers-color-scheme`). A per-user setting (`theme` in `user_settings`: `auto` by default, `light` or `dark`, on the Appearance settings page) makes the server render `data-theme="light|dark"` on `<html>` (and the matching `color-scheme` meta) on every signed-in page; `auto` renders no attribute. A second per-user setting, `accent` (also on Appearance), picks the accent from 16 Pico color families: red, pink, fuchsia, purple, violet, indigo, blue, azure, cyan, jade (the default), green, lime, yellow, amber, pumpkin, orange. Pico's neutrals are left out because slate matches the secondary buttons. The server renders `data-accent="<name>"` on `<html>` for any accent but jade, and unknown values are rejected. `accents.css` holds each family's Pico v2.1.1 primary values as `--accent-*` variables and maps them onto Pico's `--pico-primary*` variables for light, dark and follow-the-device, the same way Pico picks its theme. The CSP blocks inline styles, so the values live in that static file. In dark mode a primary button's hover background uses the family's darker light-theme shade wherever Pico's own fell below 4.5:1, so button text meets WCAG AA in every accent and theme. There is no client script, so neither the theme nor the accent flashes. The login page has no user, so it always follows the device and uses jade. Status badge colors are fixed and do not follow the accent. Apart from them, app.css uses only Pico color variables, so both themes and every accent apply to it.

---

## 12. Security

### Login (v1)

- **Username and password** on a login form, which creates a server-side session. The session cookie is `HttpOnly`, `Secure`, and `SameSite=Lax`. This is simple login rather than HTTP Basic auth: Basic auth has no logout, sends the password on every request, and handles multiple users poorly.
- Passwords are stored with argon2id.
- Failed logins are limited per (username, client IP) pair: 5 failures in 15 minutes block that pair, so failures from one source cannot lock a user out everywhere. 20 failures in 15 minutes from one client IP block that IP; an IPv6 client counts as its /64. Failures for one username from every source slow its logins down instead of blocking them: from the 10th failure in 15 minutes each attempt waits 1 second, doubling per further failure up to 30 seconds. Each attempt counts before its password is checked, so concurrent attempts cannot slip past a limit, and a successful login clears its pair's count. Usernames are at most 64 bytes. Each counter holds at most 10,000 entries and drops expired ones every minute.
- At most two password checks run at once (each argon2id check takes 64 MiB). A login that finds both busy gets `429` at once instead of waiting, and is not counted as a failure.
- CSRF protection applies to state-changing HTML routes and `/sync/actions`.
- **Sessions.** A session lasts at most 30 days from sign-in, and ends sooner when it goes unused for 7 days (its `last_seen_at`, updated on every request), so a device used once a day stays signed in. Settings → Sessions lists the active sessions and has **Sign out everywhere**, which ends every session of the user, including the current one (CSRF-protected). `siftstr user set-password` also ends them all. Queued actions survive a forced sign-out: they wait in the page's `localStorage` queue and are sent after the next sign-in.
- **Bootstrap:** on first start with an empty `users` table, Siftstr creates an admin user from `SIFTSTR_ADMIN_USER` and `SIFTSTR_ADMIN_PASSWORD`. The `siftstr user add` and `siftstr user set-password` CLI commands can also create users and reset passwords, which also gives platform wrappers (such as the StartOS package) a way to set the admin password. v1 has no other user management.
- **Behind a TLS proxy.** Inside the container every request arrives as plain HTTP, so the `Secure` cookie flag and any absolute URLs come from config (`SIFTSTR_BASE_URL`), not from `r.TLS`. Don't reject requests based on the `Host` header. The client IP for login limits is the connecting address unless that address is inside `SIFTSTR_TRUSTED_PROXIES`; then it is read from `X-Forwarded-For`, right to left, skipping trusted hops. At startup Siftstr logs a warning when `SIFTSTR_BASE_URL` is empty or not `https://`. When it is `https://`, every response also carries `Strict-Transport-Security: max-age=31536000`; browsers ignore it over plain HTTP.

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

### Request limits and timeouts

- HTML form POSTs are size-limited before parsing: `POST /login` 16 KB; `POST /logout` and `POST /settings/*` 1 MB. Over the limit gets `413`. `/sync/actions` is limited to 1 MB and `/api/*` summaries to 8 MB, each in its own handler.
- The HTTP server sets `ReadHeaderTimeout` 10s, `ReadTimeout` 60s (covers the 8 MB summaries upload), `WriteTimeout` 60s, `IdleTimeout` 120s.

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
| `SIFTSTR_BASE_URL` | none | URL the Claude task and browsers use to reach Siftstr: a local `http://host:port`, an IP, or a public `https://` domain. Used for absolute links; `Secure` cookies and the `Strict-Transport-Security` header only when it is `https://`; a startup warning is logged otherwise |
| `SIFTSTR_TRUSTED_PROXIES` | none | Comma-separated CIDRs (or single IPs) of reverse proxies. Only a request that connects from one of them has its client IP taken from `X-Forwarded-For`, read from the right and skipping trusted hops; otherwise the connecting address is the client IP. Used by the login limits |
| `SIFTSTR_SECRET_KEY` | auto-generated file | Encryption key for `connections` secrets |
| `SIFTSTR_ADMIN_USER` / `SIFTSTR_ADMIN_PASSWORD` | none | Bootstrap admin on first start |
| `TZ` | `UTC` | Default timezone for new users |

Everything else (timers, grace period, user settings, upstream connections) is set in the app and stored in the database, not in env vars.

**Backups**
- Back up the whole `/data` directory.
- SQLite runs in WAL mode, and copying the database file while Siftstr is writing can produce a broken copy. Siftstr writes a consistent snapshot once a day with `VACUUM INTO /data/backups/siftstr-YYYY-MM-DD.db` and keeps the newest 7 (Q22), so file-level backup tools (restic, borg, Nextcloud sync, StartOS) can copy `/data/backups/` without stopping the container. Snapshots hold the encrypted connection secrets but not `secret.key`, so a restore needs the key from the original `/data/secret.key` or `SIFTSTR_SECRET_KEY`.

**What the repo ships**
- A `Dockerfile` and an example `compose.yaml`: a named volume on `/data`, port 8080 published on `127.0.0.1` only, and the bootstrap env vars, intended to sit behind the operator's existing reverse proxy.
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
7. ~~**Karakeep as a source.**~~ **Decided (2026-10-11):** Siftstr polls Karakeep for unarchived bookmarks, skipping ones that are too old, already in its database (by bookmark ID or URL), or tagged `sifted` (§6). The `sifted` tag is the loop breaker. Part of the Karakeep experiment, not v1 core.
8. ~~**YouTube Watch Later.**~~ **Decided (2026-10-09):** dropped from v1. Google closed the Watch Later playlist to API writes years ago, and a self-hosted instance would also need OAuth. A user-owned playlist is a roadmap item. There is no `youtube` destination in v1, so no YouTube column in the write-back table.
9. ~~**Karakeep summary field.**~~ **Decided (2026-10-09):** Karakeep's bookmark create and update accept a `summary` field (checked against its schema in the Karakeep source), and tags are attached with a separate call. Siftstr's summary is prefixed `Agent Summary: ` and replaces only a summary that is empty or already ours; other text is kept and ours appended (changed after the first manual test). Karakeep's AI tagging stays on and Siftstr only adds its own tags (`sifted`, plus `promoted` or `kept`). **Revised (2026-10-11):** Karakeep's auto-summarize overwrites a link bookmark's summary when its crawl finishes (checked in its source), so the merge and the `Agent Summary: ` prefix are dropped. Siftstr now writes its summary in a delayed step that overwrites (§7), and a per-source `summary_source` lets a feed use Karakeep's own summary on promote (§6).
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
23. **Karakeep summary timing.** *Implemented as proposed, still to tune:* wait 30 minutes after creating a bookmark before Claude's promote summary overwrites it (`karakeep_summary_delay_minutes`), and give up waiting for Karakeep's summary on a promoted `karakeep` item after 2 hours (`karakeep_summary_timeout_hours`). Both are user settings on the Sources page. A first check (2026-10-11) saw a short page summarized within about 30 seconds, so both are generous; leave them until there are real crawl times for slow pages, and note that Karakeep re-summarizes if a bookmark is re-crawled. The poll that reads the summary back runs on the 30-minute Karakeep job rather than its own 2-minute timer (decided 2026-10-11): waiting is cheap and a failure is detected from `summarizationStatus` without waiting for the timeout.
24. ~~**Karakeep-path bookmarks start archived.**~~ **Verified and decided (2026-10-11):** untriaged items stay out of the user's main Karakeep list; keep unarchives, promote leaves it archived (and tags it), delete removes it. Checked against a real Karakeep: an archived bookmark is still crawled, summarized and tagged (§7). One consequence is built in: a create always sends the archived flag, since re-saving an existing URL would otherwise unarchive it.
25. ~~**Briefing contents.**~~ **Decided (2026-10-11):** the briefing and `/read` list titles and excerpts, plus summaries for digests and for promoted items that have one. A promoted item appears with its summary at the next rewrite (the closing pass), not live.
26. ~~**Which day an item belongs to.**~~ **Decided (2026-10-11):** an item's `batch_date` is the user's local date when it was ingested, and carryover at the morning run moves earlier days' untriaged items to Backlog or expires them as before. A promoted item whose summary arrives (Claude's, or Karakeep's) is moved to the user's current day, so it comes back in Today. Items that were waiting for a light summary under the old flow were released to `light` by migration 7, on the day they were ingested.
27. ~~**What delete does to a bookmark Siftstr did not create.**~~ **Decided (2026-10-11):** bookmarks the user saved by hand go through triage like any other, and Delete removes them too. There is no list or tag scoping. The swipe is labelled Delete, so it is clear what it does.
28. **Internal names.** *Label rename done (2026-10-11), identifiers unchanged as proposed.* The outcome is called Delete in the UI and docs, but the stored action kind (`archive`), the item state (`archived`), the sync API and the CSS still use the old name for both swipe-left outcomes (delete on the first look, archive on the second; the write-back depends on the item's state when the action arrives), and the state values already exist in shipped databases. *Proposed:* keep the identifiers until there is a reason to migrate, and rename the label, buttons and badge only. Revisit if the mismatch confuses contributors, since a rename needs an append-only migration and a sync API change.

---

## 15. Superseded decisions

These are kept for context. The current design is above.

| Earlier | Now |
|---|---|
| Checkbox triage with a submit button | Gestures and keys with three outcomes, queued per action |
| Archive as the dismiss outcome | Delete on the first look, since it removes the Karakeep bookmark where there is one; Archive only after promote, where it really is Karakeep's archive |
| Four-level tap/swipe scale | Three outcomes (archive / promote / keep) |
| Promote does no write-back | Promote saves to Karakeep (archived, tagged `sifted`, deep summary) |
| Summary written to Karakeep with an `Agent Summary:` prefix, merged with Karakeep's own text | Delayed step that overwrites the summary; a feed can instead use Karakeep's own summary on promote (`summary_source`) |
| Claude writes a light summary of every item and triage happens on it | Triage on title and excerpt; a summary appears only on promote (digests start as a summary) |
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
