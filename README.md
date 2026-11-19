# Antresol torrents addon

The torrent family of Antresol as an addon (contract 1,
`docs/addons/contract.md` in the core): searching sources, acquiring
releases, the torrent engine and its holds on library files. It is a
separate addon with its own repository and releases, not part of the core.

## Sites behind Cloudflare (FlareSolverr)

Some trackers (RuTracker, for one) sit behind Cloudflare's bot check. The
addon gets past it through [FlareSolverr](https://github.com/FlareSolverr/FlareSolverr),
a helper service its manifest declares (`helpers`). Add it to the same
`compose.yml` as Antresol, under `services:`, with no `ports:`:

```yaml
  flaresolverr:
    image: ghcr.io/flaresolverr/flaresolverr:v3.5.2
    restart: unless-stopped
```

and run `docker compose up -d`. The addon finds it by its service name
(`http://flaresolverr:8191`) within half a minute, with nothing to enter;
«Настройки → Сервер → Настройки загрузок → FlareSolverr» then says «Найден рядом с
сервером», and the sources that failed are checked again. While it is not
found, that row shows the same block.

An address wins over the one found, in this order: the one saved in that
field (saved once it answers), then the server's
`ANTRESOL_TORRENTS_FLARESOLVERR_URL`. Use one when FlareSolverr already runs
elsewhere on the network, or when the server is not in Docker; clearing the
field falls back to the next one.

## Layout

| Path | What |
| --- | --- |
| `antresol-addon.json` | the manifest (`access: "adults"`, the `files.*` hooks, the FlareSolverr helper; the build fills in `platforms` and `web`; `main.go` embeds it) |
| `main.go`, `addon.go` | the program on `addonkit`: `GET /health` (`addonkit.Ready`), the hooks at `/hooks/` |
| `internal/corecopy/` | core helper packages copied by `tools/corecopy.sh` from `tools/corecopy.list`; generated, never edited |
| `tools/corecopy.sh` | the copy (`make corecopy`) and its check (`--check`, run by `make test` and `TestCorecopyInSync` when there is a core checkout) |
| `tools/fenced.txt` | the files fenced with `//go:build ignore` until they are wired; `TestFence` keeps the list exact |
| `web/` | the web part on the SDK |
| `tools/manifest` | writes the release's `antresol-addon.json` and checks the archives |

## The core copies

`tools/corecopy.list` names core files by glob, relative to the core's
`internal/`. The script copies the non-test ones, drops a leading
`//go:build sources`, rewrites `github.com/antresol-app/antresol/internal/X`
to `github.com/antresol-app/antresol-addon-torrents/internal/corecopy/X`
and refuses a file that imports a core package not copied. After a change
in the core: `make corecopy`, then `make test`.

## Build

```sh
make test                                  # go vet, go test, corecopy --check
make corecopy                              # internal/corecopy/ anew
make build                                 # dist/
make local                                 # dist-local/torrents/
```

The program builds on `addonkit`, the module `go.mod` requires. The web
part builds on the SDK package `@antresol/addon-sdk`:
`ANTRESOL_ADDON_SDK=<its packed .tgz>`, else one packed from the core
checkout `ANTRESOL_CORE` (its `web/` must have run `npm ci`). The copied
helpers come from that checkout too (`make corecopy`); without one,
`make test` skips `corecopy --check`. `ANTRESOL_CORE` defaults to the core
repository this tree sits in, if any.

## Licence

Licence: AGPL-3.0-or-later (it includes copies of Antresol core files)

The GNU Affero General Public License, version 3 or later (see `LICENSE`).
