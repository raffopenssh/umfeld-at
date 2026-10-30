# vtcseamless-py — the Python peer package (public repo)

**https://github.com/raffopenssh/vtcseamless-py** (MIT) — `pip install git+https://github.com/raffopenssh/vtcseamless-py`

What the observed-layer peer's Python peers (and any contributor) run instead of hand-rolled scripts:

- `vtcseamless.bevdirect` — client for the Go `bevdirect-serve` (0.02° cell grid, `wait=0`
  loop, 24 h document cache), `install()`/`Server` download the release binary and run it as a
  child process. **The tile assembler stays Go** (`bevdirect@<tag>` source string; a Python
  port could never be bit-identical and would split the observer population).
- `ne_cells` — our reference package. `algo.py canon.py ns_groups.py pack.py __init__.py` are
  byte-identical to `ne_cells/` here (`scripts/ne_cells_sync_check.sh`, `REMOTE=1` checks
  GitHub). Its `__main__.py` has NO `fetch` subcommand (that is operator-only, the private prefix); ours keeps it.
- `vtcseamless.public` — public tier only: `/api/v1/ne/*`, `/api/v1/kgs`, `/api/v1/context`,
  and `POST /contrib/api/v1/ne/{kg}/report`. Never the private prefix.
- `vtcseamless observe --kg K --observer L` = cells → build → report. Verified on 63330:
  digest `594025647ec3557e` = our `bev` section (1163/1163 chunks same) from
  bevdirect-serve v0.3.0.

Rules for that repo: hosts / pins / attribution text ONLY in `vtcseamless/profile.py`
(`tests/test_wordban.py` bans the server name, private prefix, token names, `named
examples|parcel ids` everywhere else; the two frozen notice strings in `pack.py` are exempt);
no real data in the repo (synthetic village for tests). Changing a frozen ne_cells file →
bump `ALGO` per docs/ne-cells.md §FREEZE and update both repos in the same change.
