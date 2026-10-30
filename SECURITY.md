# Security policy

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting on this repository
("Security" tab → "Report a vulnerability"). Do not open a public issue for
anything that could expose data or tokens. You should hear back within a few
days; please allow a fix to ship before any public disclosure.

## Scope

- The public HTTP API (`/api/v1/…`) and the documents it serves.
- The access tiers (public / contributor / private prefix) and their token
  handling — see `docs/api-tiers.md`.
- The peer-facing write paths (`/contrib/api/v1/ne/…`).

Out of scope: the upstream data sources themselves (BEV, OSM, Statistik
Austria, RIS, EEA, INVEKOS) and the peer services the overlays talk to.

## Tokens and secrets

No token, private prefix value or credential is kept in this repository. All
of them live in the operator's environment file (`/etc/umfeld-at.env`, see
`scripts/install.sh`). If you find one committed, report it as above.

## Supported versions

Only `main` is supported; fixes land there and are deployed from it.
