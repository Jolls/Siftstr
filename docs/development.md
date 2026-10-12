# Local test mode

Run an isolated Siftstr with sample data and no upstream connections. With no connections saved, ingest has nothing to fetch and write-back is skipped (released actions are still recorded), so nothing can touch a real Miniflux, Karakeep or MeTube. The start-up log says so: `no upstream connections: nothing will be ingested or written back`.

`compose.dev.yaml` uses its own project name (`siftstr-dev`), port `8081` (bound to `127.0.0.1`) and a bind-mounted `./.local/dev-data` (git-ignored). It shares nothing with a real instance. Note that Compose also merges a `compose.override.yaml` automatically when you use plain `docker compose`; with `-f compose.dev.yaml` it does not.

## Start

```sh
docker compose -f compose.dev.yaml up -d --build
docker compose -f compose.dev.yaml logs -f
```

Open <http://localhost:8081> and log in as `admin` with `admin123` (fixed in `compose.dev.yaml`; this instance is throwaway, so the password is deliberately weak). Never add real connections to this instance.

## Seed

The seed tool is not in the released image, so run it on the host (needs Go) against the bind-mounted data directory:

```sh
go run ./cmd/seed load -data ./.local/dev-data -user admin -file cmd/seed/example.json
```

Loading the same file again changes nothing. The items come with summaries and are dated today, so they show on Today straight away; no run is needed.

**Stop the container while you seed** (`docker compose -f compose.dev.yaml stop`, then `start` again). SQLite's write-ahead log does not work across the Docker Desktop bind mount on Windows and macOS, so a seed written from the host while the container is running is invisible to the app: every list stays empty.

### Short undo hold

The default undo hold is 15 minutes. To see release and outbox behavior sooner, pass `-hold`:

```sh
go run ./cmd/seed load -data ./.local/dev-data -user admin -file cmd/seed/example.json -hold 1
```

## Simulate the morning run

Create an API key under **Settings, API keys**, then:

```sh
KEY=...   # the key
curl -s -X POST -H "Authorization: Bearer $KEY" http://localhost:8081/api/v1/runs   # note run_id and item ids
curl -s -X POST -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  http://localhost:8081/api/v1/runs/RUN_ID/summaries \
  -d '{"summaries":[{"id":"itm_123","stage":"deep","summary_md":"A canned summary."}]}'
```

Send one summary per `items[].id` from the work package. Only promoted items and digests are in it, so promote an item on Today first. Its card comes back with the summary. The full contract is in [API.md](API.md).

## What to try

- **Today and Backlog:** triage items, and check that carried-over items land in the Backlog.
- **Gestures and keys:** archive, promote and keep, by swipe, keyboard and buttons. Promoted items come back once with a deeper summary (run the morning recipe again and send a `deep` summary).
- **Undo** within the hold. Afterwards the **Activity** page shows the held actions and lets you release them now.
- **`/read`** and the markdown briefing at `./.local/dev-data/briefings/<user>/YYYY-MM-DD.md`.
- **Janitor and backup:** snapshots appear in `./.local/dev-data/backups`.

## End-to-end ingest or write-back

This mode does not test ingest or write-back. Unit tests cover them with `httptest` fakes. If you need a manual end-to-end check, run a throwaway Miniflux container with a few test feeds and point only the dev instance at it.

## Reset

```sh
docker compose -f compose.dev.yaml down
rm -rf ./.local/dev-data
```
