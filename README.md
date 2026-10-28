# grab

**Share a link from your phone, get a file that fits in the chat.**

grab is a small self-hosted web app (and installable PWA) that appears in your
phone's share sheet. Share a YouTube, Reddit, Instagram, TikTok, Facebook or
Twitter/X link to it and it downloads the video through
[MeTube](https://github.com/alexta69/metube) (yt-dlp), then re-encodes it to fit the
size limit of the app you want to send it to: Telegram, Signal, Messages or
Discord. Or keep the original file. You get a notification when it is ready.

It is multi-user: sign in through your own OpenID Connect provider and every
person gets private downloads, their own share target, their own cookies and
their own notification channel.

- Works from the Android share sheet (installable PWA), from an iPhone
  Shortcut, or from the HTTP Shortcuts app via a small authenticated API.
- Presets: `original` (shown as "Download only"), `telegram`, `signal`,
  `messages`, `discord`.
- Optional Jellyfin integration: files you mark public show up in a library.
- Single Go binary plus ffmpeg and the Apprise CLI; amd64 only.

License: AGPL-3.0-only — see [LICENSE](LICENSE).

## How it works

```
 phone ──share──▶ grab ──▶ MeTube (yt-dlp) ──▶ file on disk ──▶ pick a target
                    │                                │               │
                    │                                └─ ffmpeg re-encode to fit
                    └─ notification (Apprise) when it is ready
```

grab keeps its own records (SQLite under `DATA_DIR`); MeTube only does the
downloading. grab tells MeTube which folder to write into, so the file lands
straight in the user's private folder under `SHARES_DIR`. grab moves a file
only when its owner makes it public (or private again):

- private files: `SHARES_DIR/users/<user>/<site>/`
- public files: `SHARES_DIR/public/<site>/`

`<site>` is one of `youtube`, `reddit`, `instagram`, `tiktok`, `facebook`,
`twitter`, `other`.

Share single videos. A playlist link makes MeTube download every entry into
the folder, but grab tracks only one of them; the rest are files on disk with
no record in grab.

## Quick start

You need Docker (or Podman) with Compose and an HTTPS hostname for grab. The
PWA only installs, and so only appears in the share sheet, over HTTPS.

1. Copy `compose.example.yml` to `compose.yml` and `.env.example` to `.env`.
2. Edit `.env`: set `BASE_URL` to the public HTTPS address, and either
   configure OIDC (below) or, if you really must, `AUTH=none`.
3. Make the data directories writable by the uid in `compose.yml` (default
   `1000:1000`):
   `mkdir -p shares data cookies metube-state && chown 1000:1000 shares data cookies metube-state`
4. `docker compose up -d`
5. Put a reverse proxy with TLS in front of grab's port 8080. Do **not**
   publish MeTube's port (see [MeTube requirements](#metube-requirements)).
6. Open `BASE_URL` on your phone and install it (see [Installing on your
   phone](#installing-on-your-phone)).

The image is amd64 only. The container's health check runs `grab -healthcheck`.

## Configuration

All configuration is environment variables. Required: `AUTH` and `BASE_URL`
(plus the OIDC ones when `AUTH=oidc`). `.env.example` lists them with comments.

| Variable | Default | Meaning |
|---|---|---|
| `AUTH` | none, required | `oidc` (sign in through your identity provider) or `none` (no login, see warning). |
| `BASE_URL` | required | Public address, e.g. `https://grab.example.com`. Used for links, the OIDC redirect and notifications. No credentials, query or fragment. |
| `LISTEN_ADDR` | `:8080` | Address grab listens on inside the container. |
| `METUBE_URL` | `http://metube:8081` | Where grab reaches MeTube. |
| `SHARES_DIR` | `/shares` | Where downloaded files live (absolute path). Must be the same storage MeTube writes to. |
| `DATA_DIR` | `/data` | grab's database (`grab.sqlite3`), conversion cache and temp files (absolute path). |
| `COOKIES_DIR` | `/cookies` | Per-user site cookies (absolute path). Must be mounted at the same path in MeTube. |
| `OIDC_ISSUER` | | Issuer URL of your identity provider (required with `AUTH=oidc`). |
| `OIDC_CLIENT_ID` | | OIDC client id (required with `AUTH=oidc`). |
| `OIDC_CLIENT_SECRET` | | OIDC client secret (required with `AUTH=oidc`). |
| `OIDC_ALLOWED_GROUP` | | Group allowed to sign in (required with `AUTH=oidc`). `*` lets in anyone your provider authenticates. |
| `OIDC_ADMIN_GROUP` | | Group whose members are admins (required with `AUTH=oidc`). |
| `OIDC_SCOPES` | `openid profile email groups` | Scopes requested. Must include whatever makes your provider emit a `groups` claim. |
| `APPRISE_URLS` | empty | `AUTH=none` only: Apprise URLs for the one built-in user, separated by whitespace or commas. With OIDC, each user sets their own on the settings page. |
| `ENCODER` | `libx264` | `libx264` (software) or `vaapi` (Intel/AMD GPU). |
| `VAAPI_DEVICE` | `/dev/dri/renderD128` | Render device for `vaapi`. Pass it into the container too. |
| `HEALTHCHECK_FILE` | empty | A path relative to `SHARES_DIR`. If set, `/healthz` fails when that file is missing, which catches an unmounted network share. |
| `DELETE_AFTER_DAYS` | empty (never) | Delete private downloads older than this many days. Public files are never deleted. `0` or empty turns it off. At most `36500`. |
| `JELLYFIN_URL` | empty | Jellyfin base URL. Off unless this and the API key are both set. |
| `JELLYFIN_API_KEY` | empty | Jellyfin API key. |
| `JELLYFIN_LIBRARY_ID` | empty | Optional: refresh only this library instead of everything. |

### `AUTH=none` (not recommended)

> **Warning.** With `AUTH=none` grab has no login at all. Anyone who can reach
> the address can submit downloads, read every downloaded file and fill your
> disk. Use it only behind a reverse proxy that authenticates every request
> (forward-auth, an SSO gateway, a VPN-only listener). grab logs a warning at
> startup.
>
> There is one built-in user (admin, id `local`). Your proxy must still let
> the app's public install files through without a login, or installing it on
> a phone fails: `/`, `/index.html`, `/manifest.json`, `/sw.js`, `/app.js`,
> `/app.css`, `/icon-192.png`, `/icon-512.png` (and `/healthz` for health
> checks). Everything else, including `/api/`, must stay behind the proxy's
> authentication: with `AUTH=none` grab trusts the proxy completely and a
> request needs no token. (A grab token, `Authorization: Bearer grab_…`, is
> still checked if sent, so shortcuts work, but they must get past your
> proxy's login too. Any other `Authorization` header, such as the proxy's
> own bearer token, is ignored.)

Prefer OIDC: it is the supported way to run grab for more than one person.

## OIDC setup

1. In your identity provider create a confidential OIDC client.
2. Set its redirect URI to `<BASE_URL>/auth/callback`
   (for example `https://grab.example.com/auth/callback`).
3. Make sure it can emit a `groups` claim. The default scope list is
   `openid profile email groups`; change `OIDC_SCOPES` if your provider names
   it differently.
4. Create two groups (or reuse existing ones) and set `OIDC_ALLOWED_GROUP` (who
   may use grab) and `OIDC_ADMIN_GROUP` (who administers it). Use
   `OIDC_ALLOWED_GROUP=*` to let anyone the provider authenticates in.
5. Set `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`.

Provider notes:

- **Pocket ID:** create an OIDC client, add the redirect URI, and restrict it to
  a user group; the `groups` scope returns group names.
- **Authentik:** create an OAuth2/OpenID provider; add the `groups` scope
  mapping so the token carries a `groups` claim. The issuer is the
  provider's per-application URL.
- **Authelia:** define the client under `identity_providers.oidc.clients` with
  the `groups` scope; groups come from your user backend. Since 4.39 Authelia
  puts `groups` and `preferred_username` only in the UserInfo response, not
  the ID token; grab asks the UserInfo endpoint when the ID token lacks them,
  so this works as is. Alternatively, a `claims_policies` entry on the client
  can put them back into the ID token.
- **Keycloak:** add a "Group Membership" mapper (claim name `groups`, full
  group path off) to the client, and include it in the token.

Group names are compared as plain strings against the `groups` claim. When
the ID token has no `groups` claim (or no usable name), grab reads them from
the provider's UserInfo endpoint instead.

### When access changes take effect

grab checks group membership **only at login**. If you remove someone from the
allowed or admin group, nothing happens until their next login. Until then
their existing session (valid for 30 days) and any API tokens they created
keep working, with the access they had. To cut someone off right away, ask them
to log out, revoke their API tokens (each user manages theirs on the settings
page at `/?settings`), and remove them at your identity provider so they
cannot log in again.

## Privacy model

- **Private (default):** a download belongs to the user who made it. Only they
  can see or fetch it. It lives under `SHARES_DIR/users/<user>/`.
- **Public:** a user can publish one of their files. It moves to
  `SHARES_DIR/public/`, is visible to every signed-in user, and is the folder
  you can point Jellyfin at. If the name already exists in `public/` it gets a
  suffix rather than overwriting.
- **Admin:** members of `OIDC_ADMIN_GROUP`. Admins can see, fetch, publish
  and delete everyone's downloads, so grant it sparingly. They are also the
  only people who should reach the MeTube web UI, see below.

Two people sharing the same link at the same time is fine: MeTube processes
it once at a time and each person ends up with their own file in their own
folder. If MeTube is down, new shares are accepted as "waiting" and submitted
when it returns; the list and finished items still load.

## MeTube requirements

grab drives MeTube's API. MeTube needs:

- `CUSTOM_DIRS=true` so grab can choose the download folder per request.
- `ALLOW_YTDL_OPTIONS_OVERRIDES=true` so grab can pass per-user cookies.
- `OUTPUT_TEMPLATE=%(title).100s [%(id).40s].%(ext)s` (or any template with
  the video id in it). MeTube's default names files by title alone, so two
  different videos with the same title (Instagram calls them all "Video by
  <account>") would be written to one file and both downloads would point at it.
- The **same uid** as grab (`user:` in compose) so both can read and move the
  files.
- `COOKIES_DIR` mounted at the **same path** in both containers, **read-write**
  (yt-dlp may write refreshed cookies back).
- **No published port.** Keep MeTube on the internal Compose network that only
  grab shares.

MeTube's own web UI shows *everyone's* downloads and lets whoever uses it
override yt-dlp options, which can run commands on the host. Expose it only to
the owner, if at all (for instance a separate, authenticated route or a
local-only port), and never to grab's regular users.

### Cookies

Some sites (Instagram, age-gated or private YouTube, etc.) need a logged-in
session. Each user can upload their own `cookies.txt` for a site on the
settings page; grab uses it only for that user's downloads. Cookie slots exist
for every site except `other`.

If you, the owner, upload cookies for a site in MeTube's own UI, those are
owner-wide: they apply to **anyone** who has no cookies of their own for that
site.

## Installing on your phone

**Android:** open `BASE_URL` in Chrome, sign in, then use the menu's
"Install app". Once installed, grab appears in the share sheet; sharing a
link to it opens the app with the link filled in. (If you are not signed in
it takes you through login and back to your link.)

**iPhone:** iOS has no share-target PWAs, so use a Shortcut that calls the
API. See [docs/ios-shortcut.md](docs/ios-shortcut.md).

**Anything else (Android automation, other apps):** [HTTP Shortcuts](docs/http-shortcuts.md)
or any HTTP client.

### The API

Create an API token on the settings page (`<BASE_URL>/?settings`). It starts
with `grab_` and is shown once.

```
POST <BASE_URL>/api/downloads
Authorization: Bearer grab_...
Content-Type: application/json

{"url": "https://example.com/video", "target": "telegram"}
```

`target` is optional (`original`, `telegram`, `signal`, `messages`, `discord`);
it defaults to the user's chosen default. The response is `201` for a new
download and `200` if it already existed.

## Notifications

Each user sets their own Apprise URL(s) on the settings page (see the
[Apprise docs](https://github.com/caronc/apprise/wiki) for URL formats). grab
sends a "Ready" message when a download is ready and a "Failed" message (with
the downloader's error) when it fails. The Apprise CLI is built into the
image. With `AUTH=none` set `APPRISE_URLS` instead.

If the chosen target cannot be prepared (a conversion fails, the video is
too long, or it cannot be read), the notification explains the problem and
links to the item. The original download is kept.

If sending fails, grab retries with growing gaps (1 minute up to 1 hour) and
gives up on that message after a day.

## Jellyfin (optional)

Point a Jellyfin library at `<SHARES_DIR>/public` and set `JELLYFIN_URL` and
`JELLYFIN_API_KEY` (and `JELLYFIN_LIBRARY_ID` to refresh just that library).
grab asks Jellyfin to rescan shortly after a file is published or removed.

## Retention

`DELETE_AFTER_DAYS=30` deletes private downloads older than 30 days (checked
hourly). Public files are kept. Leave it empty to keep everything.

Deletion also stops pending conversions and removes their temporary and
finished copies. MeTube's history is kept because its URL key may already
refer to another user's newer download of the same video.

## Where secrets are stored

Protect the volumes, not just the app:

- Notification (Apprise) URLs are in `DATA_DIR/grab.sqlite3`, **unencrypted**
  (file mode 0600). They often contain tokens.
- Cookies are files under `COOKIES_DIR` (mode 0600).
- API tokens and login sessions are stored only as hashes.
- `OIDC_CLIENT_SECRET` lives in your environment; grab never logs or returns
  any of these.

Back up `DATA_DIR`; it holds all records and settings.

## Upgrading from the single-folder layout

Older single-user versions of this app (before the multi-user rewrite) kept
downloads in `SHARES_DIR/<site>/`. To adopt those files as one user's private
files:

1. The user who will own the files logs in to grab once, so grab knows them.
2. Find their folder name `<dir>`: it is shown on their settings page
   (`<BASE_URL>/?settings`, under "Account"). With `AUTH=none` it is `local`.
3. Back up `DATA_DIR` (and ideally `SHARES_DIR`).
4. Run it without `--apply` first and read the plan, then apply it:

```
docker compose exec grab grab migrate --user <dir>            # prints the plan
docker compose exec grab grab migrate --user <dir> --apply    # moves the files
```

It runs in the grab container and reads the same environment (`DATA_DIR`,
`SHARES_DIR` and `METUBE_URL`).

## Developing and testing

Go isn't needed on the host. The tests include real ffmpeg conversions:

```
podman run --rm -v "$PWD":/src:Z -v grab-gomod:/go/pkg/mod -w /src golang:1.27-alpine \
  sh -c 'apk add -q ffmpeg && SHARE_TEST_ENCODER=libx264 SHARE_REQUIRE_FFMPEG=1 go vet ./... && go test ./...'
```

`podman build -t grab:dev .` runs vet and the tests inside the build, then
builds the runtime image. Browser checks (Playwright in a container, mocked
API): `e2e/run.sh`.

`TestNoPersonalStrings` fails if any file contains a string from the
comma-separated `GRAB_DENYLIST` environment variable; it skips when unset.
