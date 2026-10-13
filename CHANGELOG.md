# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.6] - 2026-10-12
### Added
- Karakeep highlights: an opt-in setting on Settings → Sources puts one of your Karakeep highlights, with its note and a link to the bookmark, first under Today in the briefing and on `/read`. Siftstr reads your highlights when it starts and then weekly, and shows a different one each day until it has shown them all ([#34](https://github.com/Jolls/Siftstr/issues/34))

## [0.2.5] - 2026-10-12
### Added
- Settings → Appearance: choose an accent color for buttons, links and focus rings from 16 Pico color families (jade stays the default). It applies on every signed-in page and device; the sign-in page stays jade ([#36](https://github.com/Jolls/Siftstr/issues/36))
### Changed
- The Theme and list-direction settings moved from the Sources settings page to Appearance ([#36](https://github.com/Jolls/Siftstr/issues/36))
- In dark mode a primary button's hover color is a darker shade, so its text keeps 4.5:1 contrast ([#36](https://github.com/Jolls/Siftstr/issues/36))
- The badge on a triaged card is colored by status: deleted red, archived grey, promoted light green, kept light blue, with light and dark values that meet WCAG AA contrast. The badge is no longer dimmed with the rest of the card ([#31](https://github.com/Jolls/Siftstr/issues/31))

## [0.2.4] - 2026-10-12
### Added
- Settings → Sessions lists your active sessions and has a Sign out everywhere button that ends every signed-in session, including the current one ([#22](https://github.com/Jolls/Siftstr/issues/22))
- Siftstr logs a startup warning when `SIFTSTR_BASE_URL` is empty or not `https://` ([#21](https://github.com/Jolls/Siftstr/issues/21))
### Security
- A session now ends after 7 days without use, on top of the 30-day limit ([#22](https://github.com/Jolls/Siftstr/issues/22))
- With an `https://` `SIFTSTR_BASE_URL`, responses carry `Strict-Transport-Security: max-age=31536000` ([#21](https://github.com/Jolls/Siftstr/issues/21))

## [0.2.3] - 2026-10-12
### Added
- `SIFTSTR_TRUSTED_PROXIES`: comma-separated CIDRs of reverse proxies. Login limits take the client IP from `X-Forwarded-For` only for requests from those addresses ([#19](https://github.com/Jolls/Siftstr/issues/19))
### Changed
- Usernames are limited to 64 bytes ([#19](https://github.com/Jolls/Siftstr/issues/19))
- compose.yaml and compose.dev.yaml publish their port on 127.0.0.1 only, since ports Docker publishes bypass host firewalls; change the bind address to reach it from other machines ([#18](https://github.com/Jolls/Siftstr/issues/18))
### Security
- Login attempts can no longer exhaust server memory: at most two password checks run at once, and a login that arrives while both are busy gets 429. Each attempt now counts against the rate limit before its password is checked ([#17](https://github.com/Jolls/Siftstr/issues/17))
- Failed logins are counted per username and client IP, so failures from one source no longer lock a user out everywhere. Failures for a username from every source slow its logins down, up to 30 seconds per attempt, instead of blocking them ([#19](https://github.com/Jolls/Siftstr/issues/19))
- The per-IP login limit counts an IPv6 client by its /64 ([#19](https://github.com/Jolls/Siftstr/issues/19))
- The login limiter's memory is bounded: usernames are capped at 64 bytes, expired entries are swept every minute, and each counter holds at most 10,000 entries ([#19](https://github.com/Jolls/Siftstr/issues/19))
- Login and settings form posts are limited to 16 KB and 1 MB, and over-limit bodies get 413 before they are parsed. The server now has read, write and idle timeouts, so a slow client cannot hold a connection open indefinitely ([#20](https://github.com/Jolls/Siftstr/issues/20))

## [0.2.2] - 2026-10-12
### Added
- Dark mode: every page follows the device's light or dark theme, and a per-user Theme setting on the Sources settings page can force light or dark
### Changed
- Use the Pico jade accent theme; alert links no longer carry a hardcoded color fallback

## [0.2.1] - 2026-10-12
### Added
- Initial pre-release history: everything before this changelog existed is not itemized
- A `CHANGELOG.md`, and a pull request check that asks for an entry when a change affects behavior
### Changed
- A triaged card collapses to its title, source, badge and Undo. The summary and every action button except Undo are hidden until you undo it
