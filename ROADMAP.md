# Siftstr Roadmap

What gets built when. The design for all of it lives in [ARCHITECTURE.md](ARCHITECTURE.md). Items here that are "later" must still be accounted for in that design, so building them doesn't need a rewrite.

---

## v1: single user, end to end

The goal is one person's daily loop working end to end, deployed with Docker.

**Proposed build order:**

1. **Skeleton.** Go module, SQLite and migrations (multi-user schema from the start), env config, login, the bootstrap admin user, API keys, `/healthz`, and the `Dockerfile` and `compose.yaml`. It runs in Docker from the first commit.
2. **Ingest.** Mirror Miniflux feeds into `sources`, run the ingest timer, and add the per-source settings page.
3. **Claude contract.** `POST /api/v1/runs`, summary submission, the default prompt, and the scheduled-task prompt. Test it against a real Cowork run.
4. **Triage UI.** Today and Backlog pages, mobile swipes, desktop keys, the page action queue, `/sync/actions`, and the server-side hold with undo.
5. **Write-back.** The outbox, plus Miniflux (mark read), Karakeep (bookmark, tags, summary), and MeTube adapters.
6. **Briefing output.** The `/read` page, the markdown file, and the export target.
7. **Nostr ingestor and digests.**
8. **Packaging.** Multi-arch image publishing, a README with a plain-Docker deployment guide and operator requirements, and a LICENSE. The StartOS package is built separately, in its own repo, around the published image.

**v1 user model:** one user, created from env vars on first start (plus the `siftstr user add` CLI). Everything is already scoped by `user_id`.

---

## Next: multi-user

The data model, auth, scoping, and per-user connections already support multiple users from v1 (see ARCHITECTURE.md §12). This phase adds the features on top:

- Admin page: list, create, disable, and delete users, and reset passwords.
- Self-service: change password, manage own API keys, delete own data.
- Onboarding: an invite link or admin-created accounts. Open signup stays off by default.
- A per-user setup checklist (connect Miniflux, Karakeep, MeTube; create an API key; set up the Claude scheduled task).
- Per-user timezone and morning-run time shown in the UI.
- Optional: shared instance-level connections (e.g. one MeTube for everyone) that users can opt into.
- Optional: OIDC/SSO login for people who already run an identity provider.

---

## Experiment: Karakeep summaries

A per-source `summary_source` setting (`agent` or `karakeep`) decides where a promoted item's summary comes from. Triage is on title and excerpt for every feed; digests start as a Claude summary. On `agent` feeds Claude writes the summary at the next morning run and it overwrites the Karakeep bookmark's summary after a delay. On `karakeep` feeds the bookmark is created at ingest and Karakeep's own AI summary is shown on promote, with no Claude run. The aim is to try both on real feeds and see which gives the better triage experience. The same experiment adds **Karakeep as a source**: Siftstr polls Karakeep for unarchived bookmarks (its own RSS import, or anything saved there by hand) and offers them for triage, skipping ones that are too old, already known, or tagged `sifted`. Design in ARCHITECTURE.md §6 and §7, with the open timing question at Q23. Built, with a first check against a real Karakeep; the delay and timeout settings still want real crawl times. If `karakeep` wins for most feeds, the Claude path can shrink to the special cases, or give way to the local summarizer below.

---

## Karakeep highlights in the briefing

An opt-in per-user setting puts one of the user's Karakeep highlights at the top of each day's briefing and `/read`, with its note and a link to the bookmark. Karakeep has no count or random pick for highlights, so Siftstr walks the list when it starts and weekly, caches the IDs, and picks a different one each day until all have been shown. Design in ARCHITECTURE.md §8 and §10 ([#34](https://github.com/Jolls/Siftstr/issues/34)). Built; not yet checked against a real Karakeep.

## Past briefings in the app

`/briefings` lists the days the user has a briefing for, newest first, and each day can be viewed in the browser or downloaded as the stored markdown file. The days come from SQLite and the text is the file as written, never parsed. A timer writes each day's file, so it works without a Claude run ([#50](https://github.com/Jolls/Siftstr/issues/50)).

## Old Karakeep bookmarks (Old Sifting)

The Karakeep ingestor stops at the age cutoff, so older unarchived bookmarks were never triaged. A per-user setting (default 5 a day, 0 = off) brings the oldest of them into the Backlog each day, up to a Backlog size limit (default 20). Siftstr caches their IDs a few pages per run instead of reading the whole library. Design in ARCHITECTURE.md §6 and §8 ([#46](https://github.com/Jolls/Siftstr/issues/46)). Built; not yet checked against a real Karakeep.

---

## Later

- **MCP server or plugin** as an alternative to the scheduled-task API.
- **Local LLM summarizer** (Ollama, LM Studio, vLLM, or any OpenAI-compatible server) as an alternative to the Claude daily task. Siftstr sends each new item to the user's local model as it is ingested and stores the summary in the database, so there is no daily batch for this mode. It is a per-user, opt-in setting (endpoint and model stored in `connections`), and the Claude path stays as it is. Local mode needs these changes first:
  - Loosen the "No LLM calls in the app" and "One Claude run per day" invariants in AGENTS.md and ARCHITECTURE.md to cover an optional local summarizer. Hosted models are still out: no model SDKs, only a plain HTTP client to a user-run endpoint.
  - Put the summarizer behind an interface with an `httptest` fake, like every other external service.
  - Decide what Today shows while an item has no summary yet (model slow or down), and how promote's deeper summary works in this mode.
- **Rule-based filtering** (keyword, category, or author conditions) beyond the per-source settings.
- **Transcription** for podcasts and videos, so deep summaries can use the actual content.
- **More destinations** for kept items (e.g. other downloaders or podcast apps) through the `Destination` interface.
- **YouTube**: adding kept videos to a user-owned playlist (Watch Later is closed to the API; needs OAuth).
- **Reversing a write-back** when undo happens after the hold has ended (e.g. delete the Karakeep bookmark).
