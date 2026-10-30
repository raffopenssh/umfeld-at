// sqlite_driver.go — a registered SQLite driver whose per-connection PRAGMAs
// go beyond what go-sqlite3's DSN parameters expose.
//
// Why
// ───
// After the request-serving hot paths were fixed, a CPU profile under a
// 12-thread mixed load on this 2-core box attributed ~43 % of all samples to
// `database/sql.(*Rows).Next` → `sqlite3_step` (inside runtime.cgocall). The
// search index is a 21 GB file, so most of that time is SQLite copying pages
// out of the OS page cache into its own per-connection page cache.
//
// go-sqlite3's DSN supports `_cache_size` but NOT `mmap_size`, so the only way
// to set it is a ConnectHook. With mmap, SQLite reads pages directly from the
// kernel's mapping — no copy, and the mapping is SHARED across all 24 pool
// connections instead of each keeping its own private copy.
//
//	mmap_size    = 4 GB  (the box has 7.4 GB and ~5.5 GB already in page cache;
//	                      mapped pages are reclaimable, they are not RSS we own)
//	temp_store   = MEMORY (temp B-trees for GROUP BY / DISTINCT stay off disk)
package main

import (
	"database/sql"
	"log"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// sqliteTunedDriver is the driver name to use instead of "sqlite3".
const sqliteTunedDriver = "sqlite3_tuned"

// sqliteMmapBytes — 4 GiB. Pages are mapped read-only and shared by every
// connection; the kernel reclaims them under pressure, so this is not a
// hard memory commitment the way _cache_size is (that one is PER connection).
const sqliteMmapBytes = 4 * 1024 * 1024 * 1024

var registerTunedOnce sync.Once

func registerTunedSQLite() {
	registerTunedOnce.Do(func() {
		sql.Register(sqliteTunedDriver, &sqlite3.SQLiteDriver{
			ConnectHook: func(c *sqlite3.SQLiteConn) error {
				for _, pragma := range []string{
					// Order matters: mmap_size must be set before pages are cached.
					"PRAGMA mmap_size=" + itoa(sqliteMmapBytes),
					"PRAGMA temp_store=MEMORY",
					// Readers must never be blocked by the single writer.
					"PRAGMA read_uncommitted=OFF",
				} {
					if _, err := c.Exec(pragma, nil); err != nil {
						log.Printf("sqlite pragma %q: %v", pragma, err)
					}
				}
				return nil
			},
		})
	})
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
