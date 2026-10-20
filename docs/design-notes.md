# Silo x Shoko: feasibility analysis and design notes

> **Status: historical.** This is the study that preceded the plugin, kept for
> the reasoning and the citations. Its conclusion at the time was that only
> watch-state sync was worth building, and that is what shipped — see the
> [README](../README.md) for the plugin itself and
> [architecture.md](architecture.md) for how it works.
>
> Sections 1-8 analyse whether Silo could consume ShokoRelay's VFS tree
> directly. The answer was no, for the reasons recorded there: the symlink tree
> is already satisfied by the renamed source tree, and Silo's scanner does not
> accept a plugin as an identifier. Section 9 is the watch-state sync design that
> became this repository.


---

## 1. Verdict

**No new code is needed for identification or for metadata.** The lane you already have —
renamed source tree plus generated NFOs — satisfies every parser Silo has.

My previous recommendation (a title/year symlink overlay) is withdrawn. It existed to solve one
problem: putting a title and year into a folder that Silo would otherwise read as `12345`. That
problem only exists if the Silo library points at `!ShokoRelayVFS`. Point it at the renamed source
tree instead and the problem does not arise. The overlay would have added an inode per series, a
regeneration step, and a third place to keep in sync — for a result the existing tree already
produces. Your read is correct.

The one thing worth doing is a **configuration check**, not an implementation: confirm which
directory the Silo library actually points at (§5).

---

## 2. Three systems, three repos — the correction that matters

Your description is accurate, but it merged three separate tools. They are configured, updated and
debugged independently, which is why the attribution matters.

| What you called it | What it actually is | Where |
|---|---|---|
| "naming in place, tmdbid in root, anidbid in season folders" | a **LuaRenamer script** that renames the *source* files in place | `GitHub/CustomShokoRenamer/bainesrenamer.lua` |
| "ShokoRelay has an NFO generator" | a **separate Shoko plugin**, not part of ShokoRelay | `GitHub/Shoko.Plugins/Shoko.Plugin.NfoGenerator` |
| "the ShokoRelay VFS" | the symlink producer, still using bare numeric ids | `GitHub/ShokoRelay` |

ShokoRelay itself has **no naming template, no placeholders and no NFO writer**. `RelayConfig.cs`
is 535 lines with zero path-template options; the only path settings are `VfsRootPath` and
`MovieVfsRootPath` (`ShokoRelay/Config/RelayConfig.cs:470-476`). Series folders are
`Path.Combine(rootPath, folderId.ToString())` (`ShokoRelay/Vfs/VfsBuilder.cs:451`) — the AniDB id,
literally. Movie folders are the Shoko episode GUID (`VfsBuilder.cs:605-607`).

That is why the VFS lane cannot identify anything, and it is a property of ShokoRelay, not of your
configuration.

---

## 3. The naming lane — works, on the source tree

`CustomShokoRenamer/bainesrenamer.lua:17-18` documents the layout it produces:

```
Movies:  <import folder>/<title> (year) [tmdbid-id]/<title> (year).ext
Shows:   <import folder>/<title> (year) [tmdbid-id]/Season NN [anidbid-id]/<title> - SxxExx - name.ext
```

Silo reads ids out of directory names with `internal/naming/folderid.go:15`:

```go
var folderIDPattern = regexp.MustCompile(`(?i)(?:\((tmdb|tmdbid|imdb|imdbid|tvdb|tvdbid)[-=]([a-z0-9]+)\)|\[(tmdb|tmdbid|imdb|imdbid|tvdb|tvdbid)[-=]([a-z0-9]+)\]|\{(tmdb|tmdbid|imdb|imdbid|tvdb|tvdbid)[-=]([a-z0-9]+)\})`)
```

`[tmdbid-12345]` matches on the second alternative. **The spelling the renamer emits is exactly the
spelling Silo accepts.** No change needed on either side.

Two caveats:

- `anidb` is **not** in the provider alternation. `[anidbid-11061]` matches nothing and is inert
  text — see §4.2.
- Bare numbers are never ids by contract: `internal/naming/folderid_numeric_test.go:10`
  `TestParseFolderIDs_BareNumbersAreNeverIDs` covers `"86"`, `"Season 01"`, and similar. This is
  precisely why the VFS's numeric folders fail and the renamer's tagged folders succeed.

Items with no TMDB cross-reference get no `[tmdbid-]` tag at all
(`bainesrenamer.lua:402-407`). They still match: title 45 + year 20 + ids 5 + source 12 = 82,
against a floor of 55 and a single-candidate bar of 70.

---

## 4. The NFO lane — works, and carries full metadata

### 4.1 What the generator writes

`Shoko.Plugin.NfoGenerator/Nfo/NfoWriter.cs:93-95`:

```csharp
UniqueId("tmdb",  n.TmdbId,  isDefault: n.TmdbId is not null),
UniqueId("anidb", n.AnidbId, isDefault: n.TmdbId is null),
UniqueId("shoko", n.ShokoId),
```

plus `title`, `originaltitle`, `plot`, `premiered`, `year`, `rating`, `votes`, `runtime`,
`studio`, `genre`, and a Kodi `<art>` block (`NfoWriter.cs:100-117`). UTF-8, no BOM, and it only
rewrites when the content changed (`NfoWriter.cs:150-152`) — the file is its own cache.

It writes `tvshow.nfo` at the **show root** (`NfoGeneratorService.cs:385`), `movie.nfo` at the
movie root (`:324`), and episode nfos beside each video (`:354`). No `season.nfo` (`:865`).

Silo maps `<uniqueid type>` at `internal/metadata/nfo/ids.go:58-80`:

```go
case "imdb", "imdbid":                                        return idKeyIMDB
case "tmdb", "tmdbid", "themoviedb", "themoviedbid", "moviedb": return idKeyTMDB
case "tvdb", "tvdbid", "thetvdb", "thetvdbid":                 return idKeyTVDB
case "", "unknown":                                            return ""      // sniff the value
default:                                                       return "other" // names no slot
```

`type="tmdb"` lands in a trusted key. `type="anidb"` and `type="shoko"` become `"other"` and are
dropped silently.

### 4.2 `anidbid` in season folders buys nothing

Three independent blocks, all verified:

1. Not in `folderIDPattern`'s provider list (`folderid.go:15`).
2. `uniqueIDKey("anidb")` returns `"other"` (`ids.go:78`).
3. Silo has no `anidb` column or model field; `providerid/validate.go` does not know the namespace.

It is harmless — `seasonDirectoryTokenRe` still parses `Season 01 [anidbid-11061]` as season 1 —
but it is for Shoko's benefit, not Silo's. Keep it; expect nothing from it.

### 4.3 Silo uses the NFO for metadata, not just identity

`internal/metadata/nfo/nfo.go:72-100` parses title, originaltitle, sorttitle, year, plot,
premiered, genre, studio, rating and the top-level `<thumb>`/`<fanart>`. `Studios` become
`Networks` (`nfo.go:311`).

One gap: Silo does **not** parse the `<art>` block the generator writes. It finds
`poster.jpg`/`fanart.jpg` through its own local-artwork discovery instead — which is fine, because
those files are already in the source folder.

---

## 5. What to actually do

### 5.1 Check which tree the library points at

```bash
# what Silo has stored (one row tells you the root)
psql "$DATABASE_URL" -c \
  "select library_id, left(file_path, 120) from media_files limit 5;"
```

- Paths containing `!ShokoRelayVFS` → **nothing will identify**. Switch the library root to the
  renamed source tree (§5.2) or accept manual matching.
- Paths like `…/Some Show (2019) [tmdbid-12345]/Season 01 [anidbid-11061]/…` → **already correct**.

### 5.2 If the library points at the VFS

Repoint the library at the renamed source tree. That tree already has everything Silo wants:
tagged folders, a root `tvshow.nfo`, released artwork, `SxxExx` filenames.

You lose what the VFS adds on top — deduplicated episode mapping, multi-episode and multi-part
collapsing, theme shorts, Plex-tuned artwork names. If those matter, keep the VFS for Plex and use
the source tree for Silo; both can read the same files.

Verified reason the VFS cannot be fixed from the Silo side: ShokoRelay mirrors metadata files only
from the video's own directory, non-recursively (`ShokoRelay/Vfs/VfsAssetLinker.cs:44-46`,
`Directory.EnumerateFiles(dir)` with `sourceDir = Path.GetDirectoryName(locInfo.Src)` at
`VfsBuilder.cs:506`). For the renamer layout that directory is the *season* folder, so the root
`tvshow.nfo` is never mirrored. The VFS series folder therefore carries episode nfos only — which
Silo does not use for series identity. Mirrored files are otherwise safe from pruning
(`VfsBuilder.cs:441,511,747`), so the limitation is the mirror's search depth, not the cleanup.

### 5.3 Reorder, don't build

If the two trees ever disagree about an item, the fix is to re-run the renamer and the NFO
generator, not to add a layer. The renamer is the naming authority; the NFO generator is the
metadata authority; Silo is a consumer of both.

---

## 6. Is the plugin worth building at all?

Not for identification, and not for metadata that the NFO already carries. The remaining cases are
narrow:

| Capability | Value over the existing lane |
|---|---|
| Live metadata refresh from Shoko | NFOs regenerate on import/metadata-update (`NfoGeneratorSettings.cs:47,54`), so this is mostly already covered |
| Episode titles when no episode NFO exists | real, but small |
| `watch_sync_provider.v1` — push Silo watch state into Shoko | **the only genuinely new capability** |
| Artwork beyond poster/fanart | marginal; Silo reads local files |

If you want watched-state sync, that is a self-contained plugin with no VFS dependency, no
identification risk and no inode cost. Build that, and nothing from this analysis is a
prerequisite.

If the NFO lane later proves insufficient — a field Silo renders that the NFO does not carry — the
plugin becomes worth it, and the manifest/config scaffolding in Appendix B still applies.

---

## 7. Why the overlay is withdrawn

It was correct engineering for the problem as I framed it, and the framing was wrong. Recorded so
the reasoning is auditable:

1. The overlay supplied title and year to a folder Silo would read as `12345`. That is only worth
   doing if the alternative is a numeric folder. The renamed source tree is the alternative.
2. It cost one symlink per series plus a regeneration step — real, if small, ongoing maintenance
   in a third place.
3. It would have competed with the renamer for naming authority. Two systems both claiming to know
   a show's correct name is a bug waiting to happen; the renamer should win.

Had the source tree been untagged, the overlay would have been the right answer. It is not.

---

## 8. Risk notes for the existing lane

- **Tagger coverage.** Items without a TMDB cross-reference carry no `[tmdbid-]` tag. They still
  match on title + year (82), so this is cosmetic, but the NFO's `type="tmdb"` is absent for them
  too and `type="anidb"` is what remains — which Silo ignores. Expect those items to rely on the
  title/year path.
- **Folder-tag precedence.** Structured folder ids are authoritative and override scanner
  ambiguity (`internal/metadata/service.go:7480-7487`); NFO ids arrive later as hints and cannot
  override a protected stored id (`:10032-10070`). So a wrong `[tmdbid-]` in a folder name is
  sticky and needs the folder renamed, not an NFO edit.
- **Renamer/plugin ordering.** If the NFO generator runs before the renamer has moved a file, the
  NFO lands in the old directory and `SweepMisplacedShowNfos`
  (`NfoGeneratorService.cs:1668-1690`) cleans it up on a later pass. Harmless, but it explains
  stray files during initial setup.
- **One library root per physical tree.** Silo walks each physical directory once per scan
  (`internal/scanner/scanner.go:599-608`) and `media_files.file_path` is unique. Do not add both
  the source tree and the VFS as libraries if they overlap physically; with the symlink VFS they do
  not, so this only matters if you point at both.

---

## 9. Watch-state sync — separately feasible, and worth building

This is the one capability the NFO lane cannot provide. It is independent of §2-§8: no VFS
dependency, no identification risk, no inode cost.

### 9.1 The host is already a two-way reconciler

`WatchSyncProvider` splits into `ApplyEvents` (push) and `ListRemoteState` (pull), and the host
drives both (`silo-plugin-sdk/proto/silo/plugin/v1/watch_sync_provider.proto:15-22`). The service
doc states the division outright: the plugin "adapts an external watch-tracking service to Silo's
host-owned credential, outbox, retry, and reconciliation pipeline" (`:12-14`).

What the host does, so the plugin does not have to:

- **Schedules the pull** — `SyncWatchProvidersTask` runs hourly
  (`internal/taskmanager/tasks/sync_watch_providers.go:35-39`, `IntervalMs: 60*60*1000`), plus a
  manual trigger (`internal/api/handlers/watch_providers.go:181`).
- **Pushes on local events** — `POST/DELETE /watched/{id}` → `dispatchLocalWatchEvent`
  (`internal/api/handlers/items.go:713-731`) → `HandleLocalWatchEvent`
  (`internal/watchsync/service.go:377-408`), in a goroutine with a 2-minute budget.
- **Batches the rest** — `ExportHistory` sends "one bounded RPC per service iteration"
  (`internal/watchsync/plugin_provider.go:450-456`) tagged
  `WATCH_SYNC_ORIGIN_RECONCILIATION` (`:463`).
- **Prevents echo loops** — imported history is written with the provider as its source
  (`service.go:1249-1262`) and export excludes that source (`service.go:1642-1646`). Rationale,
  verbatim (`plugin_provider.go:149-156`): "HistorySource is provider-specific so importing from one
  plugin suppresses only the echo back to that same connection."
- **Writes remote state locally** — the plugin returns data; `ImportWatched` → `matchWatchedLeaves`
  (`service.go:1277-1296`) performs the match and the write.
- **Keeps cursors** — `sync_cursors jsonb` per connection and state family
  (`migrations/sql/114_watch_provider_sync_cursors.sql:4`, keys at
  `internal/watchsync/plugin_provider_state.go:17-24`).

`FetchHistory` returns nil for plugin providers by design (`plugin_provider.go:433-437`), so inbound
state flows exclusively through `ListRemoteState`.

Do **not** also register `scheduled_task.v1` — it would duplicate the hourly run, default to
startup-only until an operator binds a trigger
(`internal/plugins/task_registry.go:185-196`), and get a 10s RPC budget instead of the 2-minute
watch-sync budget (`internal/pluginhost/handshake.go:16,26-29`).

### 9.2 What the plugin implements

1. `ExchangeAPIKey` — store `base_url` + `apikey`; the key determines the Shoko user.
2. `ListRemoteState` — `GET /api/v3/Episode?pageSize=0&includeFiles=true&includeXRefs=true&includeWatched=Only`,
   map each to `WatchSyncRemoteState{provider_item_key, media, watched{play_count, last_watched_at}}`,
   `complete_snapshot: true`.
3. `ApplyEvents` — `PUT /api/v3/Episode/{id}/UserData` with `LastPlayedAt`.
4. Descriptor: `auth_methods: [API_KEY]`, `import_watched`, `export_watched`,
   `supported_media_types: [EPISODE]`, `max_batch_size` modest (Shoko writes are per-episode).

### 9.3 Three hard edges

**1. Identity namespaces are exactly `imdb`, `tmdb`, `tvdb`.** `remoteIdentityFromProto` reads only
those three literal map keys (`plugin_provider_state.go:877-879`, series at `:883`). AniDB is not a
namespace, and `provider_item_key` is opaque — it addresses the upstream record on write but does
**not** help matching. Every returned episode must therefore carry a TMDB or TVDB id, or it will
never match a Silo item. Shoko supplies `IDs.TvDB []int`, `IDs.TMDB{Episode[], Movie[], Show[]}`
and `IDs.IMDB []string` (`Shoko.Server/API/v3/Models/Shoko/Episode.cs:395-432`).

**2. Shoko has no bulk dated write.** `POST …/Watched/{bool}` stamps `DateTime.Now`
(`SeriesController.cs:2583`) and bumps `WatchedCount` on every call (`UserDataService.cs:148-175`),
so it is neither idempotent nor history-preserving. Use `PUT …/UserData` with `LastPlayedAt`
(`EpisodeController.cs:1163`, field at `Episode.cs:461`) — identical values are a no-op
(`UserDataService.cs:153-155`). Cost: one HTTP call per event.

**3. Shoko cannot push.** No webhook; notification is SignalR `/signalr/aggregate`, feed
`userData` (`API/APIExtensions.cs:715`, `SignalR/Aggregate/UserDataEventEmitter.cs:36-88`). A plugin
cannot write Silo state directly, so subscribing would only warm its own cache. Effective
Shoko→Silo latency is the hourly host poll, or Sync-now.

### 9.4 Go/no-go gate

Measure TMDB/TVDB coverage before writing code — this decides the whole project:

```bash
curl -s -H "apikey: $SHOKO_KEY" \
  'http://shoko:8111/api/v3/Episode?pageSize=0&includeXRefs=true&includeWatched=Only' \
  > /tmp/watched.json

jq '{total:      length,
     with_tvdb: ([.[] | select((.IDs.TvDB // []) | length > 0)] | length),
     with_tmdb: ([.[] | select((.IDs.TMDB.Episode // []) | length > 0)] | length)}' \
  /tmp/watched.json
```

The ratio that matters is `with_tvdb + with_tmdb` over `total`. High coverage → proceed. Low coverage
→ the sync would silently drop most of the library, and the design would need `provider_item_key`-only
matching, which this contract does not offer.

Second measurement: are those TVDB *episode* ids the same numbering Silo resolved from the folder
tag and NFO? A mismatch (absolute vs aired) produces confident wrong matches, not failures.

### 9.5 Phasing

| Phase | Scope | Effort |
|---|---|---|
| 1 | Watched only, Silo→Shoko. `ApplyEvents` + `ExchangeAPIKey`, no pull | 1-2 days |
| 2 | Watched only, Shoko→Silo. `ListRemoteState`, full snapshot | 2-3 days |
| 3 | Resume position both ways. Shoko is file-level only: read `File.ResumePosition`, write via `File/{id}/Scrobble?resumePosition=` in ticks (`FileController.cs:1541-1553`); percent needs duration | +2-3 days |
| 4 | SignalR warm-cache to cut pull cost | optional |

Phase 1 alone already delivers the capability you asked about, and is safe to ship before the
identity gate in §9.4 is answered — export has no matching step.

### 9.6 Residual risks

- **Multi-episode local files.** Silo items can span `SxxEyy-Ezz`; Shoko is strictly one episode per
  record. `MatchLeaves` expands SERIES/SEASON aggregates (`service.go:1285-1291`) but not a
  multi-episode single item.
- **Watch count inflation** if `applyCompletionRule` and the local completion threshold disagree
  (`UserDataService.cs:405-428`, default 95%) — the host sends authoritative `completed`, so honour
  it rather than recomputing from `completion_percent`.
- **Pull payload size.** A full watched snapshot includes xrefs for every episode. If Shoko exposes
  no "updated since" filter, consider `page_size` paging using `next_page_token` and set
  `complete_snapshot` only on the final page.
- **Three-way mesh.** Plex→Shoko→Silo→Shoko is safe because Shoko state is convergent (one row per
  user+episode) and Silo tags its source; but a Plex unwatch and a Silo watch racing in the same
  hour resolve to whichever write lands last, not to the newer `LastPlayedAt`.

---

## Appendix A — citation index

Renamer: `CustomerShokoRenamer/bainesrenamer.lua:17-18` (layout), `:387,400,406,428` (tag
composition).

ShokoRelay: `Config/RelayConfig.cs:470-476` · `Vfs/VfsBuilder.cs:441,451,506,511,605-607,747` ·
`Vfs/VfsAssetLinker.cs:44-46` · `Plex/PlexMapping.cs:37-40` · `Plex/PlexConstants.cs:141,150`.

NFO generator: `Nfo/NfoWriter.cs:93-95,100-117,150-152` · `NfoGeneratorService.cs:324,354,385,865,1668-1690` ·
`Config/NfoGeneratorSettings.cs:47,54`.

Silo: `internal/naming/folderid.go:15,21` · `internal/naming/folderid_numeric_test.go:10` ·
`internal/naming/season.go:28` · `internal/metadata/nfo/ids.go:58-80` · `internal/metadata/nfo/nfo.go:72-100,311` ·
`internal/metadata/service.go:1207,7480-7487,10032-10070` · `internal/scanner/scanner.go:599-608` ·
`internal/metadata/match_candidates.go:575-624` (scoring), `:159` (floor 55), `:983-987` (single-candidate bar 70).

## Appendix B — plugin scaffolding, if it is ever needed

```json
{
  "plugin_id": "shoko.vfs-metadata",
  "version": "0.1.0",
  "checksum": "__CHECKSUM__",
  "silo_api_version": "v1",
  "global_config_schema": [{
    "key": "shoko",
    "title": "Shoko Server",
    "json_schema": "{\"type\":\"object\",\"properties\":{\"base_url\":{\"type\":\"string\"},\"api_key\":{\"type\":\"string\"}},\"required\":[\"base_url\",\"api_key\"]}",
    "required": true,
    "admin_form": {"fields": [
      {"key": "base_url", "label": "Base URL", "control": "ADMIN_FORM_CONTROL_TEXT", "required": true},
      {"key": "api_key", "label": "API key", "control": "ADMIN_FORM_CONTROL_PASSWORD", "secret": true, "required": true}
    ]}
  }],
  "capabilities": [{
    "type": "metadata_provider.v1",
    "id": "shoko",
    "display_name": "Shoko",
    "description": "Looks series up by AniDB id on a Shoko Server."
  }]
}
```

`silo_api_version` must be exactly `"v1"`. `control` values are proto enums, so the full
`ADMIN_FORM_CONTROL_*` spelling is required inside a typed `admin_form`. Config arrives through
`runtime.WithConfigure(func(ctx, []*pluginv1.ConfigEntry) error)` and must not error on an empty
list — that is a fresh install.

Useful Shoko calls: `GET /api/v3/Series/AniDB/{id}/Series`,
`GET /api/v3/Series/{id}/Episode?includeFiles=true&includeAbsolutePaths=true&pageSize=0`,
`GET /api/v3/Image/{guid}`, header `apikey`. Omit `Expires` on `POST /api/v3/Auth/SignIn` for a
non-expiring token.
