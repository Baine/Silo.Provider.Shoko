# silo-plugin-shoko

[![CI](https://github.com/Baine/silo-plugin-shoko/actions/workflows/ci.yml/badge.svg)](https://github.com/Baine/silo-plugin-shoko/actions/workflows/ci.yml)

A [Silo](https://github.com/Silo-Server/silo-server) plugin that keeps watch
state in step with a [Shoko Server](https://shokoanime.com) library. It
implements the `watch_sync_provider.v1` capability, so Silo owns scheduling,
retries and reconciliation, and the plugin only translates between the two
systems.

Shoko remains the source of truth for which episodes are finished. Silo gains
them on import, and plays on either side flow to the other.

## What it syncs

| Direction | State | Write |
| --- | --- | --- |
| Shoko → Silo | watched episodes, with watch count and watched timestamp | — |
| Silo → Shoko | watched | `LastPlayedAt` on the Shoko episode record |
| Silo → Shoko | unwatched | `LastPlayedAt` cleared |
| Shoko → Silo | paused progress | the profile's continue-watching position |
| Silo → Shoko | playback | resume position while you watch, watched state on completion |

Ratings, favourites and watchlists are not synced: the provider does not
advertise those capabilities, so Silo shows no toggle for them.

## Requirements

- Silo with plugin support (`silo_api_version v1`) and at least one series or
  film library.
- Shoko Server exposing the v3 API, reachable from the Silo server, with an API
  key created under **Settings → API Keys**.
- Go 1.26 or newer, only if you build the plugin yourself.

## Install

```sh
git clone https://github.com/Baine/silo-plugin-shoko
cd silo-plugin-shoko
make build          # produces ./silo-plugin-shoko
```

Upload `./silo-plugin-shoko` in Silo under **Admin → Plugins → Upload**, then:

1. Open the plugin's settings and enter the Shoko server URL, for example
   `http://shoko:8111`, and the episode numbering your libraries use.
2. Open **Settings → Watch Providers**, choose **Shoko Server**, and paste the
   API key. Each Silo profile links its own Shoko user this way.
3. Run **Sync now** once to pull the existing history. After that it runs
   hourly.

### Silo in Docker

Silo executes the plugin inside its own container, so the binary has to match
that container's OS and architecture, not your desktop's. Take the release asset
for the container:

| Container | Asset |
| --- | --- |
| x86_64 host (`uname -m` → `x86_64`) | `silo-plugin-shoko_<version>_linux_amd64` |
| ARM host (`aarch64`, Raspberry Pi, many NAS) | `silo-plugin-shoko_<version>_linux_arm64` |

```sh
docker exec silo uname -m
```

Release binaries are built with `CGO_ENABLED=0`, so they are static and run on
Alpine-based images without extra libraries. Upload the file under
**Admin → Plugins → Upload**.

The server URL is resolved *inside* the container. `http://localhost:8111` points
at the Silo container itself, not at Shoko, so use the host's LAN address, a
container name on a shared Docker network, or the Shoko hostname — whichever the
Silo container can reach.

Prebuilt binaries are published only for the versions tagged in
[releases](https://github.com/Baine/silo-plugin-shoko/releases). `make dist`
cross-compiles linux/amd64, linux/arm64 and darwin/arm64 into `dist/` yourself.

## Configuration

The settings are split by scope.

**Plugin settings** (admin, one per installation):

| Setting | Meaning |
| --- | --- |
| Shoko server URL | Base URL of the Shoko API as the Silo server sees it |
| Episode numbering | `anidb` (default) or `tmdb` |

**Connection** (per Silo profile): the API key. Shoko API keys do not expire, so
there is nothing to refresh and nothing stored beyond the key itself.

### Episode numbering

Shoko keeps two orderings and they diverge for multi-season anime.

- `anidb` maps AniDB's absolute episode *N* to `S01E<N>`, and specials to
  `S00E<N>`. This is what a ShokoRelay `!ShokoRelayVFS` tree looks like, where
  regular episodes always sit in `Season 01`.
- `tmdb` uses the season and episode numbers TMDB placed the episode at, which
  is what a TVDB- or TMDB-numbered library expects. It needs TMDB links in Shoko
  and makes each sync slower, because every episode is returned with its TMDB
  side data.

Pick whichever matches your library layout. A mismatch corrupts nothing —
imports still match on episode-level ids and exports fall back to the same ids —
but series-plus-number resolution stops working, so more events are rejected as
unmatched.

### The toggles

All five toggles on the connection card are functional.

| Toggle | Effect |
| --- | --- |
| Export watched | a Silo play becomes `LastPlayedAt` in Shoko |
| Export unwatched | a Silo unwatch clears `LastPlayedAt`, so Shoko shows it unwatched |
| Import watched | Shoko's watched episodes become watched in Silo |
| Import paused progress | Shoko's resume position becomes the continue-watching position |
| Scrobble playback | play, pause and stop update the resume position in Shoko as you watch |

Shoko stores progress per *file*, not per episode, and clears it on its own once
a play passes 95% of the runtime. That threshold is also what turns a stop into
a watched write rather than a position near the end.

## Checking the connection

The plugin adds an admin page, **Shoko connection**, reachable from the plugin
list. It tests a server URL and API key without waiting for a sync, and reports
who the key signs in as plus the series, episode and watched counts it can see.

It exists because a failed sync cannot tell the difference between a server that
does not answer, a key that was rejected, and a URL that answers but is not the
Shoko API. The page distinguishes all three. The key is used for that one request
and is never written back into the page.

## How items are matched

Import needs no matching from the plugin: it returns Shoko's external ids and
titles and Silo's own matcher decides which library item they belong to.

Export has to go the other way, from a Silo item back to a Shoko episode, so the
plugin resolves in this order:

1. `shoko:episode:<id>` — the provider item key issued during import.
2. The series TMDB or TVDB id, plus season and episode number.
3. An episode-level TVDB, TMDB, IMDb or Shoko episode id.

First hit wins. An event that resolves to nothing is rejected as permanent
rather than retried, and appears as a count in the run's warnings.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| Run reports "no API key" | the profile has no connection yet: Settings → Watch Providers |
| Connection page says the key was rejected | key belongs to a deleted Shoko user, or was created under a different Shoko instance |
| Connection page reports a non-JSON answer | the URL points at something that is not the Shoko API, e.g. the web UI port |
| Many events rejected as unmatched | numbering mismatch, or the Silo item carries no external ids |
| Import finds watched episodes but Silo stays empty | library scan has not matched those items yet; check the item's provider ids |
| Progress import is slow | one request per part-way episode, capped at 200 per run |

## Known limits

- **Episodes and films.** An AniDB movie is matched as a film using the film's
  own provider ids; series-level state is not synced. Several watched Shoko
  entries under one film collapse into one item carrying the latest play.
- **Only `Episode` and `Special` entry types.** Credits, trailers, parodies and
  other AniDB entries are skipped and reported as a count. A film's only entry is
  of type `Episode`, so films survive the filter.
- **Watch count is not incremented on export.** The write patches
  `LastPlayedAt` alone, so Shoko's play count, tags, rating and favourite flag
  are untouched.
- **Resume position is file-level in Shoko.** There is no episode-level position
  to write, so an episode with several files keeps its position on whichever
  file the play resolved to.
- **Progress timestamps cost a request each.** Shoko does not expose when a
  resume position was written on the episode list, so the provider reads the file
  record for each part-way episode, capped at 200 per run. The rest is reported
  in the run's warnings.
- **Traversal cache.** An import run pages through a cached snapshot so a
  changing watched set cannot shift items between pages. The snapshot lives in
  the plugin process for 15 minutes.
- **No incremental sync.** Every import is a full snapshot of watched episodes;
  Shoko has no delta endpoint.

## Development

```sh
make build   # ./silo-plugin-shoko, the file Silo installs
make test
make check   # vet + tests
make dist    # cross-compiled release binaries
```

The plugin embeds `manifest.json` and stamps the running binary's SHA-256 into
it, so the host verifies the checksum it is handed. `./silo-plugin-shoko manifest` prints
the manifest exactly as the host reads it.

See [docs/architecture.md](docs/architecture.md) for the RPC surface and the
reasoning behind the write strategy, and
[docs/design-notes.md](docs/design-notes.md) for the feasibility study that
preceded the plugin.

## Licence

MIT — see [LICENSE](LICENSE).
