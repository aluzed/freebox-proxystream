# CLI reference

`proxystream` is a single binary. Run it with no arguments to start the server;
everything else is a small helper subcommand.

```
proxystream                    start the server
proxystream users              list accounts
proxystream passwd [user]      change an account password
proxystream useradd <user>     create an account
proxystream hashpass           print a bcrypt hash (rarely needed)
proxystream version            print the version
proxystream help               print usage
```

There are no flags. All configuration comes from the environment, read from a
`.env` file in the working directory if one is present. See
[`.env.example`](.env.example) for the annotated list.

---

## `proxystream`

Starts the server. Two listeners come up:

| Listener | Default | Purpose |
|---|---|---|
| Public HTTP | `HTTP_ADDR`, `0.0.0.0:8080` | The web UI and the JSON API. This is the one you open. |
| Range proxy | `INTERNAL_ADDR`, `127.0.0.1:8081` | Serves remote FTP files to `ffmpeg` as seekable HTTP. **Loopback only, never expose it.** |

```bash
cp .env.example .env
$EDITOR .env
./proxystream
```

Open `http://localhost:8080`. On a fresh database you land on `/setup` to create
the account; afterwards on `/login`.

### What it does at startup

1. Creates `CACHE_DIR`, `SUBTITLE_DIR` and `THUMB_DIR` if missing.
2. Opens the SQLite database and applies the schema (idempotent).
3. Purges expired sessions.
4. Pings the FTP server. A failure is logged as a warning and **does not stop the
   server** — the UI is still useful for fixing the configuration.
5. Reconciles the segment cache against the filesystem, in both directions:
   indexed segments whose file vanished are dropped, orphaned files are adopted,
   and leftover `.tmp` files from a hard kill are deleted.
6. Starts the encoder reaper, the cache janitor and the scan worker.

### Shutdown

`SIGINT` or `SIGTERM` triggers a graceful stop: both HTTP servers drain, every
`ffmpeg` process gets `SIGTERM` then `SIGKILL` after 5 s, and the server waits
for them to actually exit before returning. Segments already written stay on
disk and are reused on the next start.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Clean shutdown after a signal. |
| `1` | Startup failure: unreadable `.env`, invalid configuration, unwritable data directory, database that cannot be opened, or a listener whose port is taken. |

### Logging

Structured `log/slog` on stdout. `LOG_LEVEL` accepts `debug`, `info` (default),
`warn` and `error`.

```bash
LOG_LEVEL=debug ./proxystream
```

Segment requests are not logged when they succeed — they are the hot path and
would drown everything else. Encoder starts and stops always are, with the media
id, the start segment and the reason:

```
level=INFO msg="encoder started" media_id=42 variant=v1_720p_crf23_aac128_a0 start_seq=300 height=720
level=INFO msg="encoder stopped" media_id=42 variant=v1_720p_crf23_aac128_a0 start_seq=300 head=317 reason="idle timeout" elapsed=2m14s
```

Secrets are never logged: not `FTP_PASS`, not `API_TOKEN`, and never in a URL.

---

## Account commands

Accounts live in the SQLite database, never in a configuration file. The first
one is normally created from the setup page the web UI shows on a fresh install;
these commands exist for headless setups and for the day you forget the
password.

They open the database directly, so run them on the machine that holds it. Under
Docker that means `docker compose exec` (server running) or
`docker compose run --rm` (server stopped) — both see the same volume.

### `proxystream users`

```
$ proxystream users
ID  USERNAME  CREATED           PASSWORD CHANGED  ACTIVE SESSIONS
1   alex      2026-07-26 18:52  2026-07-26 19:04  2
```

`ACTIVE SESSIONS` counts unexpired login cookies, which is how you notice a
device you forgot to sign out.

### `proxystream passwd [username]`

Resets a password without needing the old one — filesystem access to the
database is the authorisation. The web UI's own form always asks for the current
password instead.

```bash
proxystream passwd              # single account: no need to name it
proxystream passwd alex         # prompts, twice, without echo
proxystream passwd alex 'a long password'   # scriptable; lands in shell history
echo 'a long password' | proxystream passwd alex
```

Every session for that account is signed out, so a forgotten laptop loses access
immediately.

### `proxystream useradd <username>`

Creates an account. Use it to provision a headless install before first contact
with the web UI:

```bash
docker compose run --rm proxystream useradd alex
```

Once any account exists, the setup page disappears and `/` goes to the login
screen.

### `proxystream hashpass`

Prints a bcrypt hash and nothing else. Kept for the rare case where you want to
write a hash into a database yourself; `useradd` and `passwd` are what you
normally want.

```bash
$ proxystream hashpass
Password:
Confirm:
$2a$10$d4iO2vT1xDLOo2xu/kCjkOzWAGZ6MzH2ag3FMgNTJvEB/zaSuy1VK
```

Input is not echoed. Prompts go to stderr and the hash to stdout, so
`> hash.txt` gives a clean value. Minimum 8 characters. bcrypt is salted, so the
same password gives a different hash every run; any of them is valid.

## `proxystream version`

```bash
$ ./proxystream version
proxystream 0.1.0
```

---

## Operational recipes

Everything below is the JSON API rather than a subcommand, since the server owns
the database while it runs. `API_TOKEN` makes these usable without a browser
session; leave it empty to disable the bearer path entirely.

```bash
export TOKEN=...   # the API_TOKEN value
export HOST=http://localhost:8080
```

Check liveness (the only endpoint that needs no authentication):

```bash
curl -s $HOST/health
```

Watch encoders, scan progress and cache usage:

```bash
curl -s -H "Authorization: Bearer $TOKEN" $HOST/api/status | jq
```

List libraries, then trigger a scan:

```bash
curl -s -H "Authorization: Bearer $TOKEN" $HOST/api/libraries | jq
curl -s -X POST -H "Authorization: Bearer $TOKEN" $HOST/api/libraries/1/scan
```

Find a title and inspect what was probed:

```bash
curl -s -H "Authorization: Bearer $TOKEN" "$HOST/api/media?q=amelie" | jq '.items[] | {id, title, status}'
curl -s -H "Authorization: Bearer $TOKEN" $HOST/api/media/42 | jq '{duration_sec, height, audio_streams, encoder_log}'
```

Write your own title and synopsis:

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"title":"Nicky Larson","overview":"Ce que vous voulez."}' \
  $HOST/api/media/42/details
```

`encoder_log` carries the last ~50 lines of `ffmpeg` stderr for that media, which
is the thing to read first when playback fails.

### Verifying the range proxy by hand

If seeking is slow, this is where to look. The exact headers are what make
`ffmpeg` treat the input as seekable:

```bash
curl -s -D- -o /dev/null -r 0-1023 http://127.0.0.1:8081/raw/42
```

You want `206 Partial Content`, `Accept-Ranges: bytes`, an exact
`Content-Length: 1024`, and `Content-Range: bytes 0-1023/<size>`. If
`Content-Length` is missing or wrong, `-ss` silently degrades to decoding from
the start and seeks crawl.

```bash
ffprobe -v error -show_format http://127.0.0.1:8081/raw/42
```

This must return quickly on a large remote file. If it hangs, the proxy is
streaming the whole file instead of serving ranges.

### Starting over

The server must be stopped first: it holds the SQLite file open.

```bash
rm -rf data/          # database, segments, posters, subtitles
```

To drop only the cache and keep the library, use **Réglages → Vider le cache**
in the UI, which stops the running encoders first and waits for them to exit.
