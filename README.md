# Siftstr

A self-hosted daily triage gate. Siftstr pulls new items from [Miniflux](https://miniflux.app) (RSS, YouTube channels, Reddit, podcasts, GitHub releases, newsletters) and from Nostr. One Claude scheduled task summarizes them each morning. You then sift each item on your phone or desktop:

- **Archive**: you're done with it.
- **Promote**: come back to it once, with a deeper summary.
- **Keep**: send it on to [Karakeep](https://karakeep.app), [MeTube](https://github.com/alexta69/metube) and so on.

It is not a reader. Source services stay the system of record, and Siftstr deletes its own workflow data after 6 months.

Siftstr makes no LLM calls itself. The summaries come from a Claude scheduled task that talks to Siftstr's JSON API once a day. See [ARCHITECTURE.md](ARCHITECTURE.md) for the design and [ROADMAP.md](ROADMAP.md) for what is planned.

## What you need

- A host that runs Docker (amd64 or arm64).
- A Miniflux account, and/or Nostr npubs to follow.
- A Claude account that can run scheduled tasks (see [docs/scheduled-task.md](docs/scheduled-task.md)).
- Optional destinations for kept items: Karakeep, MeTube.

## Deploy

The image is `ghcr.io/jolls/siftstr`, built for `linux/amd64` and `linux/arm64`. It listens on plain HTTP on port 8080 and keeps everything under `/data`.

### Docker Compose

Use [compose.yaml](compose.yaml) from this repo. Copy `.env.example` to `.env`, fill it in, and start it:

```sh
cp .env.example .env     # set SIFTSTR_BASE_URL, SIFTSTR_ADMIN_USER, SIFTSTR_ADMIN_PASSWORD
docker compose up -d --build
docker compose logs -f
```

To run the published image instead of building, swap `build: .` for `image: ghcr.io/jolls/siftstr:latest` in `compose.yaml`.

### Plain Docker

```sh
docker run -d --name siftstr --restart unless-stopped \
  -p 8080:8080 \
  -v siftstr-data:/data \
  -e SIFTSTR_BASE_URL=https://siftstr.example.com \
  -e SIFTSTR_ADMIN_USER=admin \
  -e SIFTSTR_ADMIN_PASSWORD='choose-a-long-password' \
  -e TZ=Europe/Berlin \
  ghcr.io/jolls/siftstr:latest
```

The admin variables only matter on the first start, when there are no users yet. Afterwards you can drop them. Check that it is up with `GET /healthz` (the image's `HEALTHCHECK` does this) and log in at `/login`.

### Configuration

| Variable | Default | Purpose |
|---|---|---|
| `SIFTSTR_LISTEN` | `:8080` | Listen address (plain HTTP). |
| `SIFTSTR_DATA_DIR` | `/data` | Database, key, briefings and backups. |
| `SIFTSTR_BASE_URL` | none | The URL browsers and the Claude task use to reach Siftstr. Drives absolute links and the `Secure` cookie flag. |
| `SIFTSTR_SECRET_KEY` | generated | Key that encrypts saved upstream credentials. If unset, Siftstr creates `/data/secret.key`. |
| `SIFTSTR_ADMIN_USER`, `SIFTSTR_ADMIN_PASSWORD` | none | Creates the first admin on first start. |
| `TZ` | `UTC` | Default timezone for new users. Each user can change theirs under Settings, Sources. |

Everything else (connections, timers, prompts, per-source settings) is set in the app.

Add more users with `docker compose exec` or `docker exec`:

```sh
printf '%s' 'a-password' | docker compose exec -T siftstr /siftstr user add alice
printf '%s' 'new-password' | docker compose exec -T siftstr /siftstr user set-password alice
```

### Reverse proxy and TLS

The container never terminates TLS. Put Caddy, Traefik, nginx, Tailscale or similar in front of port 8080 and forward to it.

- Set `SIFTSTR_BASE_URL` to the address people actually use. The session cookie gets the `Secure` flag only when that URL starts with `https://`, and it is never inferred from the request, so behind a TLS proxy set it to your `https://` address.
- If you serve it over plain `http://` on a LAN, the cookie is not `Secure`. That works, but anyone on that network can read the traffic.

The same URL is what the Claude task calls, so it has to be reachable from where the task runs. See [docs/scheduled-task.md](docs/scheduled-task.md).

### Trusting a private CA

If Miniflux, Karakeep or MeTube sit behind a certificate from your own CA, mount the CA certificate (PEM) into a folder and point `SSL_CERT_DIR` at it. Public CAs stay trusted.

```yaml
services:
  siftstr:
    environment:
      SSL_CERT_DIR: /certs
    volumes:
      - ./certs:/certs:ro
```

Put the CA certificate in `./certs` as a `.crt` or `.pem` file.

## Set up

1. Log in, then open **Settings, Destinations** and add your Miniflux connection (and Karakeep and MeTube if you use them).
2. Open **Settings, Sources**. Your Miniflux feeds appear after the first ingest (it runs every 30 minutes, and once at start-up). Set granularity, max depth and carryover per source.
3. For Nostr, add relays and followed npubs under **Settings, Nostr**.
4. Create the Claude scheduled task: [docs/scheduled-task.md](docs/scheduled-task.md).

### Operator requirements for Karakeep and MeTube

- **Turn off Karakeep's native Miniflux integration.** Otherwise every item reaches Karakeep whether or not you kept it. Siftstr is what decides what goes there.
- **Keep Karakeep's `CRAWLER_VIDEO_DOWNLOAD` off if MeTube handles downloads.** It is an instance-wide Karakeep setting, and Siftstr never asks Karakeep to download anything. Siftstr never downloads media itself either.

## Backups

Everything that must survive lives in `/data`:

| Path | Contents |
|---|---|
| `siftstr.db` | The SQLite database (WAL mode). |
| `secret.key` | The key that encrypts saved upstream credentials. Lose it and you re-enter them. |
| `briefings/<username>/` | The daily markdown briefings. |
| `backups/` | A consistent database snapshot written once a day, newest 7 kept. |

Don't copy `siftstr.db` with a file tool while Siftstr is running: in WAL mode that can give you a broken copy. Back up `/data/backups/` instead (restic, borg, a Nextcloud sync, or a volume snapshot), plus `secret.key` (or keep `SIFTSTR_SECRET_KEY` in your secrets store). The snapshots contain the credentials in encrypted form only.

To restore, stop Siftstr, put a snapshot at `/data/siftstr.db` (delete any `siftstr.db-wal` and `siftstr.db-shm` beside it), restore `secret.key`, and start it again.

The janitor also deletes workflow data (items, actions, outbox entries, runs) older than 6 months. Your sources, connections and settings are kept.

## Markdown briefings

Each morning's run writes `/data/briefings/<username>/YYYY-MM-DD.md`, the same content as the `/read` page. To get it into Nextcloud or Obsidian, bind-mount a synced folder over `/data/briefings/<username>`.

## Development

```sh
go build ./...
go test ./...
go vet ./...
gofmt -l .                      # must print nothing
node --test web/test/*.test.js  # client queue and gesture tests
```

Releases: pushing a tag like `v1.0.0` runs `.github/workflows/release.yml`, which builds `linux/amd64` and `linux/arm64` with `docker buildx` and publishes to `ghcr.io`. See [AGENTS.md](AGENTS.md) for the conventions and invariants.

## License

AGPL-3.0. See [LICENSE](LICENSE).
