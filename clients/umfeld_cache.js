// umfeld_cache.js — licence-compliant client cache for the umfeld-at API.
// Zero dependencies; works in browsers (fetch + localStorage/IndexedDB-free) and Node ≥ 18.
// Fetch it from GET /api/v1/docs/cache-client.js. Policy: GET /api/v1/cache/policy.
//
// Guarantees (BEV data is CC BY 4.0; BEV Nutzungsbedingungen §2.3.3 want „© BEV, JJJJ“
// on every digital copy, also after processing):
//   * attribution notice + licence URL + fetch date are stored WITH each entry
//     → render entry.creditLine wherever you show the data;
//   * ETag revalidation (If-None-Match → 304); defaults: revalidate after 24 h, discard after 7 d.
//     These are freshness RECOMMENDATIONS (CC BY has no expiry) — override via ttlSeconds /
//     maxStaleSeconds (Infinity = keep forever); the legal duty is notice + date on every copy;
//   * 202 / pending:true / ready:false / warming{} / non-200 are NEVER cached;
//   * entries with an osm{} block are flagged hasOsm (ODbL share-alike on redistribution).
//
// Usage:
//   import { UmfeldCache } from "./umfeld_cache.js";
//   const cache = new UmfeldCache({ baseUrl: "https://example.org" });
//   const e = await cache.get("/api/v1/context", { lon: "15.4395", lat: "47.0707" });
//   console.log(e.data, e.creditLine);
//   // optional persistence: new UmfeldCache({ baseUrl, storage: window.localStorage })

const DEFAULT_TTL_S = 24 * 3600;
const MAX_STALE_S = 7 * 24 * 3600;
const UNCACHEABLE = ["/api/v1/debug/", "/api/v1/feedback"];
const OSM_NOTICE = "© OpenStreetMap contributors, ODbL 1.0 (https://www.openstreetmap.org/copyright) — share-alike if redistributed as a database";
const FALLBACK_NOTICE = "© BEV – Bundesamt für Eich- und Vermessungswesen, CC BY 4.0, bearbeitet";

function isPending(b) {
  if (!b || typeof b !== "object") return false;
  if (b.pending === true || b.ready === false || "warming" in b) return true;
  const m = b.meta;
  return !!(m && typeof m === "object" && (m.pending === true || "warming" in m));
}
function hasOsm(b, depth = 0) {
  if (depth > 3 || !b || typeof b !== "object") return false;
  if (Array.isArray(b)) return b.slice(0, 200).some((x) => hasOsm(x, depth + 1));
  if (b.osm && typeof b.osm === "object") return true;
  return ["data", "results", "features", "rows", "items"].some((k) => k in b && hasOsm(b[k], depth + 1));
}
async function sha256Hex(s) {
  if (globalThis.crypto?.subtle) {
    const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
    return [...new Uint8Array(d)].map((x) => x.toString(16).padStart(2, "0")).join("");
  }
  const { createHash } = await import("node:crypto");
  return createHash("sha256").update(s).digest("hex");
}

export class Entry {
  constructor(rec, fromCache, forceStale) {
    this.data = rec.data;
    this.notice = rec.notice || FALLBACK_NOTICE;   // X-Data-Attribution
    this.licenseUrl = rec.licenseUrl;
    this.fetchedAt = rec.fetchedAt;                // ms since epoch
    this.etag = rec.etag;
    this.fromCache = fromCache;
    this.stale = forceStale || Date.now() - rec.fetchedAt > 120_000;
    this.hasOsm = !!rec.hasOsm;
  }
  get stand() { return new Date(this.fetchedAt).toISOString().slice(0, 10); }
  get osmNotice() { return this.hasOsm ? OSM_NOTICE : ""; }
  /** One line for a map footer / export header. */
  get creditLine() {
    return `${this.notice} · Stand: ${this.stand} · nicht amtlich` + (this.hasOsm ? " · " + OSM_NOTICE : "");
  }
}

export class UmfeldCache {
  /**
   * @param {{baseUrl:string, ttlSeconds?:number, maxStaleSeconds?:number, storage?:Storage|null, prefix?:string}} opts
   * storage: any object with getItem/setItem/removeItem (localStorage, or your own adapter).
   */
  constructor({ baseUrl, ttlSeconds = DEFAULT_TTL_S, maxStaleSeconds = MAX_STALE_S, storage = null, prefix = "umfeld:" }) {
    this.base = baseUrl.replace(/\/$/, "");
    this.ttl = ttlSeconds * 1000;
    this.maxStale = maxStaleSeconds * 1000;
    this.storage = storage;
    this.prefix = prefix;
    this.mem = new Map();
    if (storage) storage.setItem(prefix + "ATTRIBUTION", this.attributionText());
  }

  attributionText(notice = FALLBACK_NOTICE) {
    return `This store caches responses of the umfeld-at API.\n${notice}\n` +
      "Licence: CC BY 4.0 — https://creativecommons.org/licenses/by/4.0/\n" +
      "Data is MODIFIED (re-projected, re-assembled from tile clips, simplified, enriched); not an official extract; " +
      "not affiliated with BEV. Show each entry's fetchedAt date with the data. Entries with hasOsm=true contain " +
      "OpenStreetMap-derived fields (ODbL) and must not be merged into a redistributed CC BY database.\n" +
      `Policy: ${this.base}/api/v1/cache/policy`;
  }

  async get(path, params) {
    const qs = params ? "?" + new URLSearchParams(params).toString() : "";
    const url = this.base + path + qs;
    const key = await sha256Hex(url);
    const cacheable = !UNCACHEABLE.some((p) => path.startsWith(p));
    const rec = cacheable ? this._load(key) : null;
    const now = Date.now();
    if (rec && now - rec.fetchedAt < this.ttl) return new Entry(rec, true, false);

    const headers = {};
    if (rec?.etag) headers["If-None-Match"] = rec.etag;
    let resp;
    try {
      resp = await fetch(url, { headers });
    } catch (err) {
      if (rec && now - rec.fetchedAt < this.maxStale) return new Entry(rec, true, true);
      throw err;
    }
    if (resp.status === 304 && rec) {
      rec.fetchedAt = now;
      this._store(key, rec);
      return new Entry(rec, true, false);
    }
    if (!resp.ok) {
      if (rec && now - rec.fetchedAt < this.maxStale) return new Entry(rec, true, true);
      throw new Error(`umfeld-at API ${resp.status} for ${url}`);
    }
    const body = await resp.json();
    const link = resp.headers.get("Link") || "";
    const m = link.match(/<([^>]+)>;\s*rel="license"/);
    const fresh = {
      data: body,
      notice: resp.headers.get("X-Data-Attribution") || FALLBACK_NOTICE,
      licenseUrl: m ? m[1] : "https://creativecommons.org/licenses/by/4.0/",
      fetchedAt: now,
      etag: resp.headers.get("ETag") || "",
      hasOsm: hasOsm(body),
    };
    // Never persist UNKNOWN (202/pending/warming) or errors.
    if (cacheable && resp.status === 200 && !isPending(body)) this._store(key, fresh);
    return new Entry(fresh, false, false);
  }

  _load(key) {
    if (this.mem.has(key)) return this.mem.get(key);
    if (!this.storage) return null;
    const raw = this.storage.getItem(this.prefix + key);
    if (!raw) return null;
    const rec = JSON.parse(raw);
    if (Date.now() - rec.fetchedAt > this.maxStale) { this.storage.removeItem(this.prefix + key); return null; }
    this.mem.set(key, rec);
    return rec;
  }
  _store(key, rec) {
    this.mem.set(key, rec);
    if (!this.storage) return;
    try {
      this.storage.setItem(this.prefix + key, JSON.stringify(rec));
      this.storage.setItem(this.prefix + "ATTRIBUTION", this.attributionText(rec.notice));
    } catch { /* quota exceeded: keep in memory only */ }
  }
}
