# bevdirect — moved to the public repo

The BEV-tile assembler that used to live here is now open source:

**https://github.com/raffopenssh/vtcseamless** (MIT; data CC BY 4.0 — see its README)

- Module path: `github.com/raffopenssh/vtcseamless`. The root package is the
  generic tile-stitching engine (own-tile clip, guarded polyclip union, ring
  classification, seam merge); **`bevdirect/`** is the BEV preset (download +
  cache, MVT decoding, enrichment, cell-cached `Service`, embedded VGD admin
  table) and keeps the package name `bevdirect`.
- Binaries: `cmd/bevdirect`, `cmd/bevdirect-serve`; deploy per `DEPLOY.md`
  (public `bootstrap.sh`, no credentials).
- **The NE source string `bevdirect@<Version>` is unchanged.** `v0.3.0` is the
  first public tag (module-path change ⇒ new baseline tag); its cell output was
  verified byte-identical to the last pre-public build (`bevdirect-v0.2.1` in
  this repo's history) on five viewports with and without enrichment. Tags are
  plain `v*` from now on (no `bevdirect-` prefix).
- **Tiles are RAM-only (since the commit after v0.3.0, tag v0.3.1):** raw
  `.pbf` bytes live in a bounded, TTL-expiring in-memory LRU (`TileCache`,
  `-tile-cache-mb`, default 1 GiB ≈ ~200 KGs of mixed terrain); nothing is ever
  written to disk (`TestNoTileFilesOnDisk`). `-cache` is accepted and ignored.
  The NE builders (`vtcseamless observe`, `ne_cells build --bevdirect`) only read
  tiles through one running bevdirect-serve's `/viewport`, so keep **one** server
  alive for the whole batch — a restart starts cold.
- Nothing in it derives from this repository's index; the server does not
  import it.

```go
import "github.com/raffopenssh/vtcseamless/bevdirect"
```
