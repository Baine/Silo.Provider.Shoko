# Changelog

All notable changes to this plugin. The version is the one in `manifest.json`,
which the host records per installation.

## 0.4.2

- Rename the build output and release assets to `silo-plugin-shoko`, so a
  download is not called `plugin`. No functional change: Silo renames the
  installed binary to `plugin` regardless.
- README documents installing into a Silo Docker container, including picking
  the asset that matches the container's architecture.
- Repository and Go module renamed to `silo-plugin-shoko`; publisher shown as
  Baine.

## 0.4.1

- Fix the connection test: the form posted to an absolute path and landed on the
  Silo start page, and the route was only declared for `GET`, so the submit had
  no descriptor. The form now posts to its own URL and the manifest declares both
  `GET` and `POST` for `/connection-test`.
- Depend on the published plugin SDK (v0.28.0) instead of a local checkout.

## 0.4.0

- Import paused progress: Shoko resume positions become the profile's
  continue-watching position, ordered by the file's own timestamp.
- Export playback: play, pause and stop update the resume position in Shoko;
  a stop at or past the completion threshold marks the file watched.
- Admin connection-test page, so an unreachable server, a rejected key and a URL
  that is not the Shoko API can be told apart.
- README documents all five connection toggles; both previously inert ones now
  do something.

## 0.3.0

- AniDB movies sync as films rather than as a series with one episode.
- Specials are imported alongside regular episodes.

## 0.2.1

- Fix series paging: the Shoko series endpoint caps `pageSize` at 100, so the
  previous 1000 was rejected and left the export index empty.
- Request AniDB side data on the episode query, which the entry-type filter and
  the numbering need. Without it every episode was skipped.

## 0.2.0

- Move the server URL and numbering to the plugin's global settings; the
  connection now carries only the API key. Saving settings restarts the plugin,
  and a fresh install sends no settings at all.

## 0.1.0

- First release: watched-state import and export against Shoko's v3 API via a
  single-field JSON patch, with snapshot paging and a `numbering` setting.
