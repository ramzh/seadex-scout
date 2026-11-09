# The built-in dashboard

This fork serves its own dashboard from the container, on port `9119`. You don't need Grafana, Loki, Alertmanager or a log collector. Open `http://<host>:9119`.

## How it works

seadex-scout reports everything through its structured log, as [Monitoring and alerts](monitoring.md) describes. The upstream Grafana dashboard reads those lines back out of Loki. The built-in dashboard reads the same lines inside the process instead. A logging handler copies each line into the dashboard's memory before writing it to standard output as usual.

As a result, the two dashboards show the same numbers, the scout's checks are unchanged, and the container log stays the same. You can still load the Grafana dashboard and the Loki rules alongside it.

The page refreshes every 30 seconds, but only from the container's own memory. It never contacts SeaDex. Only the scout's check schedule does: in this fork's starter config, a quick check every hour (`poll_interval: "1h"`), one of which is the daily full check. The attention tiles scale with `poll_interval`. **Last check** allows three intervals, as the container's health check does. A lasting problem counts as cleared once one and a half intervals pass without it being logged again.

The dashboard reads the lines at `info` even when `log.level` is `warn` or `error`, so raising the level does not empty it.

Its state goes in `webui-state.json` beside `config.yaml`, saved every minute and at shutdown. After a restart it shows the last findings, library overview and health until the next check replaces them. Delete the file to start over.

## Tabs

- **Overview**:
  - Attention tiles, shown only while something needs a look. They use the same conditions as the Grafana tiles and Loki alerts.
  - Count tiles for **Upgrades**, **Not downloaded**, **Groups to check** (several release groups, or a group that can't be read) and **No SeaDex best**. Clicking one filters the table to that kind, and clicking it again shows everything. Then the share of your anime at SeaDex's best or an alt, and when the last check ran.
  - The **Library overview** bars from the daily full check.
  - A filter bar for both tables:
    - title or group, Sonarr or Radarr, and what to show;
    - **Show optional upgrades**, which adds the upgrades where you already have a SeaDex alt;
    - **Show unmonitored (incl. Specials)**, off by default, which adds series, seasons and movies set to unmonitored in Sonarr or Radarr. That includes Specials (season 0), which Sonarr leaves unmonitored on most shows, so films and specials filed there stay hidden until it is ticked. **Scout health → Hidden as unmonitored** lists every hidden row and the flag that hides it.

    The dashboard reads the monitored flags straight from the Sonarr and Radarr APIs (`/api/v3/series` and `/api/v3/movie`, or `/api/v5` on Sonarr 5) every 15 minutes, so a change there shows without waiting for a SeaDex check. A row counts as monitored until its flags have been read.
  - **Upgrades available** lists everything with something to do. The **Why** column says what:
    - Upgrades come first: **Newer version** of your group, **Neither best nor alt**, and optionally **SeaDex alt**.
    - Then **Not downloaded**: seasons and films Sonarr or Radarr has, with no file yet, that SeaDex has an entry for.
    - Then the release groups that need a look: **Several release groups**, and undecided comparisons. Those say which side has no group: **Your group can't be read** (your files name none) or **SeaDex best has no group** (SeaDex's best is an untagged NOGRP release).
    - The original app leaves a comparison undecided when either side has no group. This fork still decides it when your group is one SeaDex names. If it's a named best group, the row is at SeaDex best. If it's a named alt group, the row is a **SeaDex alt** optional upgrade, as with Space Brothers' Dekinai against an untagged best.
    - Then **No SeaDex best**: entries where SeaDex names only a theoretical best, the entry is incomplete, or the best is filtered out or on a tracker you have turned off. The SeaDex best column says which, and the alt still shows.
  - **Already on a SeaDex pick** lists what you hold at SeaDex's best, or at an alt where SeaDex names no best. A negative alt size change there is disk space the alt would save.

  Both tables have the same columns:
  - **On disk:** the size of the files Sonarr or Radarr holds.
  - SeaDex's best and its headline alt each get a **Size / change** column and a **Link** column, Nyaa first. The size is the download, and the change below it is how much your library grows or shrinks if you take it.
  - Both group names link to the show's SeaDex page.
  - When SeaDex lists more than one best or alt release, each further one gets a sub-row directly under the row, in the SeaDex best or SeaDex alt columns. It has its own name, info line (such as `best · remux · 1080p · dual-audio`), size and change, and Nyaa, → qBit and → Prowlarr buttons. The same release posted on Nyaa and AnimeBytes counts as one, with both links. Up to 6 bests and 6 alts are shown per entry. Alt groups with no release you can get, such as AnimeBytes-only ones with `animebytes` off, are still named under the alt as "also …".
  - An incomplete SeaDex entry with a best still lists as an upgrade, marked "SeaDex entry incomplete".
  - A size change is worked out by matching each episode the download holds to the file Sonarr has for it, then subtracting the files it would replace. When the files can't be matched one by one, for example because of a naming style the scout can't read, a release that covers more or fewer episodes than you have, or a season Sonarr numbers differently, the change falls back to the download minus the **On disk** size. That fallback is marked **≈**.

  To list all of this, this fork's comparison runs in an extended mode (`compare.Config.Extended`). It also reports entries upstream stays silent on: no file, already at best, and no obtainable best. It adds attributes to every finding line: `library_bytes`, `entry_incomplete`, `no_best_reason`, the `alt_*` ones, and the sizes. The new rows log at `info` as `SeaDex entry not downloaded`, `library holds the SeaDex best` and `SeaDex lists no best you can get`. None of this is in the log contract upstream's alert rules read, apart from one change: an incomplete entry with a best now logs `better release available` rather than `SeaDex entry is incomplete`. The log grows by one line per SeaDex entry in your library on each check.
- **Activity**: upgrades entering and leaving the list.
- **Scout health**:
  - Lasting problems such as a SeaDex outage.
  - The configuration in effect, a chart of full-check durations, errors and degraded checks.
  - Torznab feed traffic.
- **Reports**: every report in `report.dir`, viewable in the page with verdict filters, or downloadable as Markdown or JSON. **Run a report now** starts `seadex-scout report` inside the container, the same as `docker exec seadex-scout /seadex-scout report`.
- **Log**: the newest 500 log lines, minus the per-finding and Torznab request lines.

## Settings

| Variable | Description | Default |
| --- | --- | --- |
| `SEADEX_SCOUT_WEBUI_PORT` | Port to listen on. `off` turns the dashboard off. | `9119` |
| `SEADEX_SCOUT_WEBUI_BIND` | Address to listen on. Empty means every interface. | _(empty)_ |
| `SEADEX_SCOUT_WEBUI_PASSWORD` | Set it to require HTTP Basic sign-in. | _(unset)_ |
| `SEADEX_SCOUT_WEBUI_USERNAME` | User name for the sign-in. | `admin` |

These are environment variables rather than `config.yaml` keys. Upstream rejects unknown keys in `config.yaml`, so keeping the dashboard settings out of it leaves the file compatible both ways.

`GET /healthz` answers `ok` without a sign-in, for a reverse proxy's health check.

## Refresh local library

The scout reads your whole Sonarr and Radarr library only during the daily full check, so a release you download and import can stay listed for up to a day. **↻ Refresh local library**, at the right of the filter bar, shortens that:

- It re-reads Sonarr and Radarr straight away, and compares them against the SeaDex catalogue the last full check downloaded, which the scout now keeps in memory.
- It also re-reads which series, seasons and movies are monitored first, so **Show unmonitored** reflects a change at once instead of within the usual 15 minutes. That part runs even when the library refresh can't, such as while a check is running.
- **Nothing is fetched from SeaDex.** The only traffic is to your own Sonarr and Radarr. Brand-new series can still mean an AniList lookup or an anime ID map update, as in any check.
- Upgrades you've since imported move to **Already on a SeaDex pick** or drop off.
- It takes about as long as reading your library does. The status beside the button shows its progress, and the tables update when it ends.
- It uses the same lock as the scheduled checks, so it never runs at the same time as one. If a check is running, it says so; try again when that finishes.
- It needs one full check since the container started, because that is where the catalogue comes from. Right after a start, the button says to wait.
- The daily full check still runs on its own schedule. It is what picks up new SeaDex entries and changes.
- It logs `local library refresh started` and `local library refresh complete`, not `reconcile started` and `reconcile complete`, so it is not counted as a full check. It does not rebuild the Torznab feed or count as a SeaDex read for outage tracking.

## Library and Queue tabs

These tabs act on Sonarr and Radarr directly, through the URLs and API keys in `config.yaml`.

- **Library** lists every monitored season and film whose files score below the minimum custom format score (`SEADEX_SCOUT_MIN_SCORE`, default `5000`, the score a SeaDex best grab reaches with the custom formats in [the indexer guide](torznab-indexer.md)). The scan runs a minute after the start and every 6 hours, or on **↻ Scan scores**. Each row is joined with the scout's finding:
  - **Fix flags** shows where the scout says you already hold the SeaDex best. It gives that season's files from the best group the `Freeleech25` + `Scene` flags a best grab carries (Radarr: `G Freeleech25` + `G Scene`), so the custom format scores them. Files from any other group are left alone, and the server takes the groups from its own finding, not from the page.
  - **Search** shows where SeaDex lists a better release, and starts a season, series or movie search.
  - The buttons above the table do the same for every row shown. Bulk searches go one a minute.
- **Queue** lists Sonarr's and Radarr's queues. **Import** (Sonarr) previews, then imports a finished download as hardlinks, the way Manual Import does. It takes only files matched to episodes with no file or a file below the minimum, overrides only the "episode was unexpected" check, and gives the files the grab's indexer flags, which Manual Import drops. **Remove** drops a download from the queue without touching the torrent or blocklisting it.
- Overview rows get the same **Search** and **Fix flags** buttons and a badge with their low-score count. **ⓘ** beside a title opens everything about that series.

## Notifications

The container can send notifications to [ntfy](https://ntfy.sh) by itself, without Loki or Alertmanager. Set `SEADEX_SCOUT_NTFY_URL` to your topic's full address, such as `https://ntfy.sh/my-seadex-topic` or the address of your own ntfy server, and subscribe to that topic in the ntfy app. On the public server, anyone who knows a topic name can read it, so pick a long, hard-to-guess name or protect it with an access token.

| Variable | Description | Default |
| --- | --- | --- |
| `SEADEX_SCOUT_NTFY_URL` | The topic's full address. Empty turns notifications off. | _(unset)_ |
| `SEADEX_SCOUT_NTFY_TOKEN` | An access token (`tk_...`) for a protected topic. | _(unset)_ |
| `SEADEX_SCOUT_NTFY_USERNAME`, `SEADEX_SCOUT_NTFY_PASSWORD` | A user for a protected topic, instead of a token. | _(unset)_ |
| `SEADEX_SCOUT_NTFY_EVENTS` | What to send, comma-separated. `digest` is a daily summary at 09:00 local time of searches, grabs and flag fixes. | `upgrades,problems,errors,reports,digest` |
| `SEADEX_SCOUT_NTFY_OPTIONAL` | `true` also sends upgrades where you already have a SeaDex alt. | `false` |
| `SEADEX_SCOUT_NTFY_UNMONITORED` | `true` also sends upgrades for unmonitored series, seasons and movies. | `false` |
| `SEADEX_SCOUT_DASHBOARD_URL` | The dashboard's address, for a Dashboard button on each message. | _(unset)_ |

The events:

- **upgrades**: one message per new upgrade or newer version, when a check first finds it. It gives the groups, the best and alt sizes and size changes, and buttons for Nyaa (or the release's tracker), Sonarr or Radarr, and the dashboard. Tapping the message opens the SeaDex entry. Upgrades found right after a start are not sent, so a restart or an image update sends nothing. More than 10 in one check send the first 10 and a summary.
- **problems**: a lasting problem, such as SeaDex being unreachable or the library coming back much smaller, when it starts and when it ends. It also sends when no check has finished or started in three check intervals (at least 3 hours), and when checks run again. A problem is announced once, and a restart does not repeat it.
- **errors**: a one-off error, such as a failed Sonarr read. The same error sends again at most every 6 hours.
- **reports**: a report you started from the dashboard finished.

These follow the same conditions as upstream's alert rules (see [Monitoring and alerts](monitoring.md#alerting)), so you get the useful part of that setup from the container alone. The **Notifications** card on the Scout health tab shows where messages go, how many were sent and the last error, and has a **Send test** button.

## Send to qBittorrent

With `SEADEX_SCOUT_QBIT_URL` set, every Nyaa link in the tables gets a **→ qBit** button underneath. It adds that release's `.torrent` to qBittorrent under the category set for Sonarr or Radarr, depending on which app the row belongs to. Use the categories your Sonarr and Radarr qBittorrent download clients use, so they pick the download up and import it as they would one of their own grabs.

| Variable | Description | Default |
| --- | --- | --- |
| `SEADEX_SCOUT_QBIT_URL` | qBittorrent's Web UI address, such as `http://192.168.1.10:8080`. Empty hides the buttons. | _(unset)_ |
| `SEADEX_SCOUT_QBIT_USERNAME`, `SEADEX_SCOUT_QBIT_PASSWORD` | The Web UI sign-in. Leave them empty if qBittorrent skips sign-in for local addresses. | _(unset)_ |
| `SEADEX_SCOUT_QBIT_CATEGORY_SONARR` | The category for Sonarr rows. It is created if missing. | _(none)_ |
| `SEADEX_SCOUT_QBIT_CATEGORY_RADARR` | The category for Radarr rows. It is created if missing. | _(none)_ |

How it behaves:

- The button sends the Nyaa link of the release shown, and qBittorrent downloads the `.torrent` from Nyaa itself.
- When a SeaDex pick is a set of single-episode torrents rather than a season pack, the link, and so the button, is for one of them only.
- The server only accepts `https://nyaa.si/view/<number>` links and only "sonarr" or "radarr" from the page, so the button can't be used to make qBittorrent fetch anything else.
- Use the server's LAN address for qBittorrent. A Tailscale `*.ts.net` name does not resolve inside the container.
- A button that has added its release shows **Added ✓** until the page is reloaded. A failure shows **Failed – retry**, with qBittorrent's reason on hover.

## Search Prowlarr and send to SABnzbd

With Prowlarr and SABnzbd set up, every Nyaa link also gets a **→ Prowlarr** button. It does the usenet route by hand-search for you:

1. It reads the release title at the top of the Nyaa page, such as `[hchcsen] Aim for the Top! GunBuster & DieBuster (Top o Nerae 1 & 2!) [BD 1080p x264 Hi10P FLAC]`. That is the page's heading, not a folder or file name from its file list.
2. It searches Prowlarr for exactly that text, across every indexer Prowlarr has, and keeps the usenet results. Sites such as AnimeTosho repost Nyaa releases to usenet under the same name.
3. A panel lists the results. Those whose title matches exactly, ignoring case, spacing and punctuation, are marked **✓ exact** and come first, with indexer, size, age and grabs.
4. **Send** adds the one you pick to SABnzbd, under the category set for the row's app, so Sonarr or Radarr imports it as one of its own.

| Variable | Description | Default |
| --- | --- | --- |
| `SEADEX_SCOUT_PROWLARR_URL` | Prowlarr's address, such as `http://192.168.1.10:9696`. | _(unset)_ |
| `SEADEX_SCOUT_PROWLARR_KEY` | Prowlarr's API key. The optional indexer can use the same one. | _(unset)_ |
| `SEADEX_SCOUT_SABNZBD_URL` | SABnzbd's address, such as `http://192.168.1.10:8080`. | _(unset)_ |
| `SEADEX_SCOUT_SABNZBD_KEY` | SABnzbd's API key (not its NZB key). | _(unset)_ |
| `SEADEX_SCOUT_SABNZBD_CATEGORY_SONARR`, `SEADEX_SCOUT_SABNZBD_CATEGORY_RADARR` | The category each app's releases go to; it must exist in SABnzbd. | SABnzbd's default |

The buttons show only when all four addresses and keys are set. Some things to know:

- Each click is one live Prowlarr search, which can take a while and counts toward your indexers' API limits, as a search by hand does.
- The panel never sends anything by itself.
- The page picks a result by its place in a search the server made in the last 30 minutes, never by address, so it cannot make SABnzbd fetch anything else.
- Release titles are kept in memory, so searching the same Nyaa release again does not fetch its page again.
- The **Prowlarr and SABnzbd** card on the Scout health tab has a **Test connection** button. It checks both API keys and that the two categories exist in SABnzbd.
- Use LAN addresses. A Tailscale `*.ts.net` name does not resolve inside the container.

## Limits

- The dashboard runs in daemon mode only. A container started with `mode: report` exits after its report and serves nothing.
- With `poll_interval: off`, checks run in `docker exec` children whose log lines never reach the daemon. The dashboard then stays mostly empty, the same as the Grafana dashboard.
- **First seen** resets when the container restarts, as upstream.
- Torznab request counts start from when the dashboard was first installed.

## Security

The page loads nothing from outside the container and sends a strict Content-Security-Policy. It escapes every value from SeaDex, Sonarr and Radarr, and links only to `http` and `https` addresses. A report run can only be started by the page itself, through a custom request header that another site cannot send. Without a password, anyone who can reach the port can see your library's findings and start a report, so keep the port on your local network or set `SEADEX_SCOUT_WEBUI_PASSWORD`.

## Working on the page

`internal/webui/static` holds the page: plain HTML, CSS and JavaScript, built into the binary. To see it with made-up data, without Sonarr:

```sh
WEBUI_DEMO_ADDR=127.0.0.1:9119 go test -run TestDemo -timeout 0 ./internal/webui/
```
