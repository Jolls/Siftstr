# Siftstr JSON API

This is the contract between Siftstr and the daily Claude scheduled task. Two calls make up a run. The browser's `/sync/actions` endpoint is separate and will be documented with the triage UI.

## Authentication

Every request carries a per-user API key:

```
Authorization: Bearer <key>
```

Create one with `siftstr apikey create <username>`. The key identifies the user, so everything below is scoped to that user. A key is accepted on `/api/*` only. Session cookies are not accepted there.

Errors are JSON: `{"error": "message"}` with a matching status (`401` for a missing or bad key, `404` for an unknown run, `400` for a malformed body).

`GET /api/v1/ping` returns `{"user": "<username>"}` and is useful for checking a key.

## `POST /api/v1/runs`

Starts a run. No body. Siftstr fetches nothing from upstream. It:

1. applies carryover to earlier days' untriaged items: sources set to `drop` expire them, sources set to `carry` keep them in the Backlog;
2. bundles pending items of digest sources into one digest per source;
3. builds the work package from what is already in the database.

Returns `201` with the work package. Calling it again before summaries arrive is safe: the same pending work is offered again, digests are reused rather than duplicated, and a repeat call within an hour reuses the open run, so you get the same `run_id` back. Runs that never receive summaries are deleted after 7 days.

```jsonc
{
  "run_id": "run_8f3c...",        // pass back when submitting summaries
  "day": "2026-10-09",            // the user's local date for this run
  "timezone": "Europe/Berlin",    // the user's setting; all stored times are UTC
  "instructions": {               // user prompts, or the shipped defaults
    "global": "...", "light": "...", "deep": "...", "digest": "..."
  },
  "items": [
    {
      "id": "itm_123",
      "stage": "light",           // "light" | "deep"
      "media_type": "article",    // article | video | podcast | post
      "source": { "id": "src_9", "name": "Some Blog", "prompt_override": null },
      "title": "...", "url": "https://...", "excerpt": "...",
      "content": null             // light: always null. deep: the full content when Miniflux provided it
      // deep items also carry "light_summary": the summary shown when it was promoted
    }
  ],
  "digests": [
    {
      "id": "dig_7",
      "source": { "id": "src_4", "name": "r/news", "prompt_override": "..." },
      "entries": [ { "title": "...", "url": "...", "excerpt": "..." } ]
    }
  ]
}
```

How to use it:

- For each `light` item write a light summary from the title and excerpt, following `instructions.global` and `instructions.light`, then the source's `prompt_override` if it has one.
- For each `deep` item write the deeper summary, following `instructions.deep`.
- For each digest write one summary of all its entries, following `instructions.digest`.
- Items from disabled sources are never included.
- Deep items come first, then light items newest first. A package holds at most 200 individual items. Anything over that stays pending and goes out in a later run.
- A digest holds at most 100 entries, newest first. Entries over that stay pending and go into the next digest.

## `POST /api/v1/runs/{run_id}/summaries`

```jsonc
{
  "summaries": [
    { "id": "itm_123", "stage": "light", "summary_md": "..." },
    { "id": "itm_456", "stage": "deep",  "summary_md": "..." },
    { "id": "dig_7",   "stage": "light", "summary_md": "..." }   // digests use stage "light"
  ]
}
```

Returns `200`:

```jsonc
{
  "accepted": 3,
  "skipped": [ { "id": "itm_999", "reason": "not waiting for a light summary" } ]
}
```

- A summary is stored only if the item or digest belongs to the caller, was sent by this run, and is still waiting for that stage. Anything else is listed in `skipped` and changes nothing.
- Sending the same batch twice is harmless: the second time everything is skipped.
- An item that gets no summary stays pending and is offered again at the next run.
- `summary_md` is Markdown, 1 to 20,000 characters. The request body is limited to 8 MB.
- `404` if `run_id` does not exist or belongs to another user. `413` if the body is over 8 MB; send fewer summaries per call.
- A stored light summary moves the item to `light` and places it in the list for the run's day (`day` in the work package), even if the summaries arrive after midnight. A deep summary moves it to `deep`. A digest summary moves the digest and the entries that run sent to `light`.
