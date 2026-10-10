# The Claude scheduled task

One Claude scheduled task per user runs each morning. The prompt below is deliberately short and stable. Everything that changes (tone, per-source instructions) arrives inside the work package, so you edit prompts in Siftstr's settings and never in the task.

## Setup

### 1. Make Siftstr reachable from the task

The task is an HTTP client: it calls `POST <BASE_URL>/api/v1/runs` from wherever Claude runs it, so that machine has to be able to reach Siftstr. Set `SIFTSTR_BASE_URL` to the address the task will use. Siftstr builds the work package's absolute URLs from it and does not check whether it is reachable. Which route you use is your call:

| Setup | `SIFTSTR_BASE_URL` looks like | Use it when |
|---|---|---|
| LAN address | `http://192.168.1.20:8080` | the task runs on a machine inside your network, such as a desktop app on the same LAN |
| VPN or overlay network (Tailscale, WireGuard) | `http://siftstr.tailnet-name.ts.net:8080` | the task runs on a device that is on the same VPN |
| Reverse proxy with TLS on a public name | `https://siftstr.example.com` | the task runs somewhere that cannot join your network, such as Claude's cloud |
| Tunnel (Cloudflare Tunnel, a reverse tunnel) | `https://siftstr.example.com` | you don't want to open a port, but the task needs to reach in from outside |

If the task's host can't resolve or route to the address, the run fails at step 1 with a connection error. `GET <BASE_URL>/api/v1/ping` with the key is a quick way to check from that host. Anything public should be HTTPS: the API key is a bearer token.

### 2. Create an API key

Each user has their own key; it identifies the user, so the task only ever sees that user's items. Either:

- open **Settings, API keys** in the app and create one; or
- run the CLI. With Docker Compose:

  ```sh
  docker compose exec siftstr /siftstr apikey create <username> --label claude-task
  ```

  or with plain Docker, `docker exec siftstr /siftstr apikey create <username> --label claude-task`.

The key is shown once, so copy it now. `siftstr apikey list <username>` shows keys without their secrets, and `siftstr apikey revoke <username> <key-id>` revokes one.

### 3. Create the scheduled task

Create a scheduled task in Claude, scheduled for the morning (the day boundary is per user, set at the top of **Settings, Sources**). Give it the base URL and the key in the prompt below, and paste the prompt. Keep the key out of anything you share.

### 4. Check it

Run the task once by hand. The reply should say how many summaries were accepted. Then open Siftstr: the items should now be in **Today**. If a run fails, nothing is lost: pending items stay pending, and the next run summarizes everything still waiting.

## Prompt

```
You are the morning summarizer for Siftstr. Base URL: <BASE_URL>. API key: <KEY>.
Send the key as "Authorization: Bearer <KEY>" on every request.

1. POST <BASE_URL>/api/v1/runs. The response is a JSON work package.
2. Write a summary for every entry in "items" and "digests". Follow
   "instructions.global" plus the instruction for the entry's stage
   ("light", "deep", or "digest" for digests), then the entry's
   source.prompt_override if it is not null. Use only what the package gives
   you, except where a deep instruction tells you to fetch the page.
3. POST <BASE_URL>/api/v1/runs/<run_id>/summaries with
   {"summaries": [{"id": "...", "stage": "light" | "deep", "summary_md": "..."}]}.
   Digests use their "dig_" id with stage "light".
4. Report how many summaries were accepted and list any skipped.

If a step fails, stop and report the error. Do not retry in a loop.
```

The full request and response shapes are in [API.md](API.md).
