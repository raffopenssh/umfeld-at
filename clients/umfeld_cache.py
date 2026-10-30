"""umfeld_cache.py — licence-compliant client cache for the umfeld-at API.

Zero dependencies (stdlib only). Copy this file into your project, or fetch it
from GET /api/v1/docs/cache-client.py. Policy: GET /api/v1/cache/policy.

What it guarantees (BEV data is CC BY 4.0; BEV Nutzungsbedingungen §2.3.3 want
„© BEV, JJJJ“ on every digital copy, also after processing):
  * the attribution notice + licence URL + fetch date are stored WITH each entry
    and returned to you — render `entry.notice` wherever you show the data;
  * ETag revalidation (If-None-Match → 304) — fresh at zero bytes;
  * TTL defaults: revalidate after 24 h, discard after 7 d. These are freshness
    RECOMMENDATIONS (CC BY has no expiry) — override via ttl=/max_stale=; the
    legal duty is only that every copy shows notice + date;
  * 202 / pending:true / ready:false / warming{} / non-200 are NEVER cached
    (they mean "unknown", not "no data");
  * `ATTRIBUTION.txt` is written beside any on-disk cache;
  * rows carrying an osm{} block are flagged `entry.has_osm` (ODbL share-alike
    if you redistribute them as a database).

Usage:
    from umfeld_cache import UmfeldCache
    cache = UmfeldCache(base_url="https://example.org", dir="./umfeld-cache")
    e = cache.get("/api/v1/context", {"lon": "15.4395", "lat": "47.0707"})
    print(e.data, e.notice, e.stale)
    # Display/export: always include e.notice (and e.osm_notice if e.has_osm).
"""
from __future__ import annotations

import gzip
import hashlib
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Optional

DEFAULT_TTL = 24 * 3600          # recommended_ttl_s from /api/v1/cache/policy (advisory)
MAX_STALE = 7 * 24 * 3600        # recommended_max_stale_s (advisory; None = keep forever)
UNCACHEABLE_PREFIXES = ("/api/v1/debug/", "/api/v1/feedback")
OSM_NOTICE = "© OpenStreetMap contributors, ODbL 1.0 (https://www.openstreetmap.org/copyright) — share-alike if redistributed as a database"


@dataclass
class Entry:
    data: Any
    notice: str            # „© BEV, JJJJ – Datenquelle: … CC BY 4.0, bearbeitet“ (X-Data-Attribution)
    license_url: str
    fetched_at: float      # unix seconds — render as "Stand: YYYY-MM-DD"
    etag: str
    from_cache: bool
    stale: bool            # older than the server max-age; still dated, still attributed
    has_osm: bool

    @property
    def stand(self) -> str:
        return time.strftime("%Y-%m-%d", time.gmtime(self.fetched_at))

    @property
    def osm_notice(self) -> str:
        return OSM_NOTICE if self.has_osm else ""

    @property
    def credit_line(self) -> str:
        """One line for a map footer / CSV header / README."""
        s = f"{self.notice} · Stand: {self.stand} · nicht amtlich"
        return s + (" · " + OSM_NOTICE if self.has_osm else "")


def _is_pending(body: Any) -> bool:
    if isinstance(body, dict):
        if body.get("pending") is True or body.get("ready") is False or "warming" in body:
            return True
        meta = body.get("meta")
        if isinstance(meta, dict) and (meta.get("pending") is True or "warming" in meta):
            return True
    return False


def _has_osm(body: Any, depth: int = 0) -> bool:
    if depth > 3:
        return False
    if isinstance(body, dict):
        if isinstance(body.get("osm"), dict):
            return True
        return any(_has_osm(v, depth + 1) for k, v in body.items() if k in ("data", "results", "features", "rows", "items"))
    if isinstance(body, list):
        return any(_has_osm(x, depth + 1) for x in body[:200])
    return False


class UmfeldCache:
    def __init__(self, base_url: str, dir: Optional[str] = None, ttl: int = DEFAULT_TTL,
                 max_stale: Optional[int] = MAX_STALE, user_agent: str = "umfeld-cache.py/1"):
        self.base = base_url.rstrip("/")
        self.ttl = ttl
        self.max_stale = max_stale if max_stale is not None else float("inf")
        self.ua = user_agent
        self.dir = dir
        self.mem: dict[str, dict] = {}
        self.policy_notice = ""
        if dir:
            os.makedirs(dir, exist_ok=True)
            self._write_attribution_file()

    # ── public ────────────────────────────────────────────────────────────
    def get(self, path: str, params: Optional[dict] = None) -> Entry:
        url = self.base + path + ("?" + urllib.parse.urlencode(params, doseq=True) if params else "")
        key = hashlib.sha256(url.encode()).hexdigest()
        cacheable = not path.startswith(UNCACHEABLE_PREFIXES)
        rec = self._load(key) if cacheable else None
        now = time.time()

        if rec and now - rec["fetched_at"] < self.ttl:
            return self._entry(rec, from_cache=True)

        headers = {"Accept-Encoding": "gzip", "User-Agent": self.ua}
        if rec and rec.get("etag"):
            headers["If-None-Match"] = rec["etag"]
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                status, hdrs, raw = resp.status, resp.headers, resp.read()
        except urllib.error.HTTPError as e:
            if e.code == 304 and rec:
                rec["fetched_at"] = now            # renewed at zero bytes
                self._store(key, rec)
                return self._entry(rec, from_cache=True)
            if rec and now - rec["fetched_at"] < self.max_stale:
                return self._entry(rec, from_cache=True, force_stale=True)
            raise
        except (urllib.error.URLError, TimeoutError):
            if rec and now - rec["fetched_at"] < self.max_stale:
                return self._entry(rec, from_cache=True, force_stale=True)
            raise

        if hdrs.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
        body = json.loads(raw) if raw else None
        notice = hdrs.get("X-Data-Attribution", "")
        try:  # http.client decodes headers as latin-1; undo if the server sent UTF-8
            notice = notice.encode("latin-1").decode("utf-8")
        except (UnicodeEncodeError, UnicodeDecodeError):
            pass
        lic = ""
        for part in hdrs.get_all("Link") or []:
            if 'rel="license"' in part:
                lic = part.split(">")[0].lstrip("<")
        rec = {"data": body, "notice": notice, "license_url": lic, "fetched_at": now,
               "etag": hdrs.get("ETag", ""), "has_osm": _has_osm(body)}
        # Legal + correctness: never persist UNKNOWN or errors.
        if cacheable and status == 200 and not _is_pending(body):
            self._store(key, rec)
        return self._entry(rec, from_cache=False)

    # ── internals ─────────────────────────────────────────────────────────
    def _entry(self, rec: dict, from_cache: bool, force_stale: bool = False) -> Entry:
        age = time.time() - rec["fetched_at"]
        return Entry(rec["data"], rec["notice"], rec["license_url"], rec["fetched_at"], rec.get("etag", ""),
                     from_cache, force_stale or age > 120, rec.get("has_osm", False))

    def _path(self, key: str) -> str:
        return os.path.join(self.dir, key[:2], key + ".json.gz")

    def _load(self, key: str) -> Optional[dict]:
        if key in self.mem:
            return self.mem[key]
        if not self.dir:
            return None
        p = self._path(key)
        if not os.path.exists(p):
            return None
        with gzip.open(p, "rt", encoding="utf-8") as f:
            rec = json.load(f)
        if time.time() - rec["fetched_at"] > self.max_stale:
            os.remove(p)
            return None
        self.mem[key] = rec
        return rec

    def _store(self, key: str, rec: dict) -> None:
        self.mem[key] = rec
        if not rec["notice"]:
            rec["notice"] = self.policy_notice
        if not self.policy_notice and rec["notice"]:
            self.policy_notice = rec["notice"]
            self._write_attribution_file()
        if self.dir:
            p = self._path(key)
            os.makedirs(os.path.dirname(p), exist_ok=True)
            with gzip.open(p, "wt", encoding="utf-8") as f:
                json.dump(rec, f, ensure_ascii=False)

    def _write_attribution_file(self) -> None:
        if not self.dir:
            return
        notice = self.policy_notice or "© BEV – Bundesamt für Eich- und Vermessungswesen, CC BY 4.0, bearbeitet"
        with open(os.path.join(self.dir, "ATTRIBUTION.txt"), "w", encoding="utf-8") as f:
            f.write(
                "This directory caches responses of the umfeld-at API.\n"
                f"{notice}\n"
                "Licence: CC BY 4.0 — https://creativecommons.org/licenses/by/4.0/\n"
                "Data is MODIFIED (re-projected to WGS84, re-assembled from tile clips, simplified, enriched);\n"
                "not an official extract; not operated by or affiliated with BEV. Each entry carries fetched_at —\n"
                "show that date wherever you display the data. Entries with has_osm=true contain OpenStreetMap-derived\n"
                "fields (© OpenStreetMap contributors, ODbL) and must not be merged into a redistributed CC BY database.\n"
                f"Policy: {self.base}/api/v1/cache/policy · Details: {self.base}/api/v1/license\n"
            )
