# Architecture

The plugin is a Go binary that Silo runs out-of-process and talks to over gRPC.
Everything it does is in service of one capability, `watch_sync_provider.v1`, so
the host keeps ownership of the parts that are easy to get wrong: the sync
schedule, the outbox, retries, cursors, and suppressing echoes of its own writes.

```
Silo host ──gRPC──▶ plugin ──HTTP──▶ Shoko Server /api/v3
```

## Process shape

`main.go` embeds `manifest.json` and serves three gRPC services:

| Service | Purpose |
| --- | --- |
| `Runtime` | `GetManifest` and `Configure` |
| `WatchSyncProvider` | the seven sync RPCs |
| `HTTPRoutes` | the admin connection-test page |

The checksum in the manifest is overwritten at startup with the SHA-256 of the
running binary, because the host verifies it before installing. `./silo-plugin-shoko
manifest` prints the result.

## Configuration plumbing

Two scopes, deliberately separated.

**Global** (`global_config_schema`, key `shoko`) holds the server URL and the
numbering. The host delivers it to `Configure` as a list of `ConfigEntry`
messages, one per schema key, before any capability call, and again after an
admin saves. `Configure` must succeed on an empty list, because a fresh install
sends none; defaults are applied in code, never read back from the manifest.
Saving settings restarts the plugin process, so nothing can be cached across a
settings change.

**Connection** holds the API key, which is per Silo profile and per Shoko user.
It arrives on every RPC inside `WatchSyncProviderConfig` and is treated as a
credential, not as configuration: it is never logged, never rendered, and never
cached beyond the request that used it.

The host flattens a connection's config into `values["<schemaKey>.<field>"]`, and
rejects any key the schema did not declare.

## Import: `ListRemoteState`

Silo asks for a page, gets items back, and keeps asking until it receives a
cursor. Two things matter here.

**Snapshots, not live reads.** The first page of a traversal fetches the whole
watched set from Shoko, caches it in the plugin process under an opaque
traversal id, and serves `"<id>:<offset>"` page tokens. The alternative — a fresh
Shoko query per page — lets an episode watched mid-traversal shift the offsets,
and Silo would read the gap as "removed from the provider", i.e. an unwatch.
Snapshots expire after 15 minutes and at most four are retained.

**`complete_snapshot` is only set on the last page**, so Silo never treats a
partial traversal as authoritative.

Each item carries a `provider_item_key` of `shoko:episode:<id>` or
`shoko:movie:<id>`, plus whatever external ids Shoko knows. The host persists the
key, which is what makes export resolution reliable later.

Films are exported as `WATCH_SYNC_MEDIA_TYPE_MOVIE`. Shoko models an AniDB movie
as a single episode of type `Episode`; the plugin detects it through the AniDB
anime type and reports it as a film so it matches Silo's movie library items
rather than polluting a series.

## Export: `ApplyEvents`

Events arrive in order and each is resolved to a Shoko episode:

1. the provider item key,
2. series TMDB/TVDB id plus season and episode,
3. an episode-level TVDB/TMDB/IMDb/Shoko id.

Resolution 2 and 3 need a mapping from Silo's identifiers to Shoko's, so the
plugin builds an index of the Shoko library on demand and caches it for five
minutes (at most two retained). The index is keyed by episode-level ids, by
series id plus season and episode, and by provider key.

### Why the write is a JSON patch

`PATCH /api/v3/Episode/{id}/UserData` with
`[{"op":"add","path":"/LastPlayedAt","value":<ts|null>}]` and
`Content-Type: application/json-patch+json`.

Shoko's controller starts that patch from the user's existing record, then applies
the operations. Three consequences, all of them the reason for the choice:

- **Play counts survive.** `PUT` would take a body whose `PlaybackCount` defaults
  to zero, resetting the count, and `POST .../Watched/{true}` stamps the current
  time and inflates the count. Neither is acceptable for a sync.
- **Rewriting the same timestamp is a no-op.** Shoko only saves when the watched
  state actually changed, so a repeated export does not create a duplicate play.
- **`null` clears the state.** The update type records that `LastPlayedAt` was
  set even when the value is null, which is what makes unwatching work.

Watch count is never incremented by the plugin.

## Progress

Shoko stores a resume position on the *file* record, not on the episode, and
clears it itself once a play passes 95% of the runtime.

The host asks for progress with `state_kinds`, and the plugin answers from a
separate traversal: episodes whose resume position is part-way. The percentage
comes from the position against the episode's runtime. The ordering timestamp
comes from the file record's `LastUpdatedAt`, read per episode — Shoko exposes no
progress timestamp on the episode list — capped at 200 lookups per run, with the
remainder reported in the warnings.

Scrobble events write to `PATCH /api/v3/File/{id}/Scrobble` with the position in
ticks. A stop at or past the completion threshold, or a completed session, is
sent as a watched write without a position, which is how Shoko marks the file
finished and clears the resume point.

## Failure mapping

Every RPC returns a `WatchSyncFault` whose `safe_message` is written for an
operator and never contains credentials or response bodies.

| Condition | Code | Effect in Silo |
| --- | --- | --- |
| transport error, 5xx, timeout | `TEMPORARY` | retried with backoff |
| 429 | `RATE_LIMITED` | retried after `retry_after` |
| 401 / 403 | `INVALID_CREDENTIAL` | sync stops, connection marked invalid |
| 400 from Shoko | `INVALID_REQUEST` | that event only |
| event resolves to nothing | `REJECTED` with `PERMANENT` | counted in warnings |
| operation not supported (favourites, ratings) | `REJECTED` with `INVALID_REQUEST` | counted in warnings |

A per-event rejection never fails the batch; only a fault on the whole response
does.

## The connection-test page

An `http_routes.v1` handler that renders plain HTML with no scripts and a strict
`Content-Security-Policy`. The route is declared twice in the manifest — `GET`
navigable for the page, `POST` for the form — because the host's proxy matches
method and path exactly and has no fallback. The form deliberately has no
`action` attribute: an absolute path would resolve against the Silo origin and
land on the SPA shell instead of the plugin route.

The page answers the one question a failed sync cannot: whether the problem is an
unreachable server, a rejected key, or a URL that is not the Shoko API.
