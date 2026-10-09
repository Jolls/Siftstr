# The Claude scheduled task

One Claude scheduled task per user runs each morning. The prompt below is deliberately short and stable. Everything that changes (tone, per-source instructions) arrives inside the work package, so you edit prompts in Siftstr's settings and never in the task.

## Setup

1. Make Siftstr reachable from where the task runs, and set `SIFTSTR_BASE_URL` to that address. It can be a local address with a port (`http://192.168.1.20:8080`) or an external IP or domain. You choose; Siftstr does not check.
2. Create an API key under Settings, API keys (or with `siftstr apikey create <username> --label claude-task`). The key is shown once.
3. Create a scheduled task in Claude, scheduled for the morning, and give it the base URL and the key.
4. Paste the prompt below.

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
