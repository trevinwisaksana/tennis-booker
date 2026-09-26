# booker

Auto-books courts at eliteclub.saraga.id at the 06:00 WIB release (target = release day + 3).

```
./run.sh check      # cookie sources exist and contain a *_session cookie
./run.sh selftest   # build, then BOOKER_SELFTEST=1 for accounts A and B
./run.sh start      # start A (06:00) and B (07:00) in background, wait for "armed"
./run.sh status | stop | wait
```

Account A books 06:00-07:00, account B 07:00-08:00, both trying courts `372,373`
(Indoor 2, then Indoor 3). Logs: `booker-a.log`, `booker-b.log`; response pages
are saved as `a-*.html` / `b-*.html` next to them.

## Auth

Preferred: the site's own email/password login (the /login form). Set
`BOOKER_EMAIL_A` / `BOOKER_PASSWORD_A` and `BOOKER_EMAIL_B` / `BOOKER_PASSWORD_B`.
Google-only accounts can set a site password via "Forgot password" on /login.
The daemon logs in at start and again whenever the session dies.

Alternatively, cookies:

The site's session cookie is `saraga_indonesia_v2_session` (8-day lifetime), not
`laravel_session`. Copy the whole `cookie:` request header from DevTools
(Network tab, reload /booking, first "booking" row).

- Mac: `pbpaste > cookie.txt` (account A), `pbpaste > cookie2.txt` (account B).
- Cloud: set `BOOKER_COOKIE_A` / `BOOKER_COOKIE_B` as environment variables;
  these take precedence over the files.

Don't log out in the browser afterwards; that invalidates the copied session.

## Env

`BOOKER_EMAIL`+`BOOKER_PASSWORD`, `BOOKER_COOKIE` or `BOOKER_COOKIE_FILE`, `BOOKER_START_TIME`, `BOOKER_COURT_IDS`,
`BOOKER_LABEL`, `BOOKER_SMTP_USER`/`BOOKER_SMTP_PASS`/`BOOKER_EMAIL_TO`,
`BOOKER_SELFTEST=1`, `BOOKER_DRY_RUN=1`, `BOOKER_ONESHOT=1` (exit after one release),
`BOOKER_TARGET_DATE=YYYY-MM-DD`, `BOOKER_FIRE_AT=<RFC3339>` (testing),
`BOOKER_RETRY_WINDOW` (default 30s of confirmation retries after release).

`process_booking` is never retried; the result is verified via GET /booking.
