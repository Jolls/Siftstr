# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- Settings → Sessions lists your active sessions and has a Sign out everywhere button that ends every signed-in session, including the current one.
- Siftstr logs a startup warning when `SIFTSTR_BASE_URL` is empty or not `https://`.
- Dark mode: every page follows the device's light or dark theme, and a per-user Theme setting on the Sources settings page can force light or dark.
- Initial pre-release history: everything before this changelog existed is not itemized.
- A `CHANGELOG.md`, and a pull request check that asks for an entry when a change affects behavior.
- `SIFTSTR_TRUSTED_PROXIES`: comma-separated CIDRs of reverse proxies. Login limits take the client IP from `X-Forwarded-For` only for requests from those addresses.

### Changed

- Usernames are limited to 64 bytes.
- compose.yaml and compose.dev.yaml publish their port on 127.0.0.1 only, since ports Docker publishes bypass host firewalls; change the bind address to reach it from other machines.
- Use the Pico jade accent theme; alert links no longer carry a hardcoded color fallback.
- A triaged card collapses to its title, source, badge and Undo. The summary and every action button except Undo are hidden until you undo it.

### Fixed

### Removed

### Security

- A session now ends after 7 days without use, on top of the 30-day limit.
- With an `https://` `SIFTSTR_BASE_URL`, responses carry `Strict-Transport-Security: max-age=31536000`.
- Login attempts can no longer exhaust server memory: at most two password checks run at once, and a login that arrives while both are busy gets 429. Each attempt now counts against the rate limit before its password is checked.
- Failed logins are counted per username and client IP, so failures from one source no longer lock a user out everywhere. Failures for a username from every source slow its logins down, up to 30 seconds per attempt, instead of blocking them.
- The per-IP login limit counts an IPv6 client by its /64.
- The login limiter's memory is bounded: usernames are capped at 64 bytes, expired entries are swept every minute, and each counter holds at most 10,000 entries.
- Login and settings form posts are limited to 16 KB and 1 MB, and over-limit bodies get 413 before they are parsed. The server now has read, write and idle timeouts, so a slow client cannot hold a connection open indefinitely.
