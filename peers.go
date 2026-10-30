// peers.go — deployment configuration: this service's public URL, the private
// prefix and the sibling services whose data the NE product joins or links to.
// Everything here comes from the environment (EnvironmentFile of the systemd
// unit, see scripts/install.sh); the code ships neutral defaults so the
// repository names no host.
//
//	UMFELD_PUBLIC_URL        scheme+host of this service (share links, UA, attribution)
//	UMFELD_PRIVATE_PREFIX    path prefix of the private tier (default /k)
//	UMFELD_TOKEN(_NAMES)     private-tier tokens / their names (api_tiers.go)
//	CONTRIBUTOR_TOKEN(_NAMES) report-only peer tokens (api_ne_reports.go)
//	NE_WRITER                token name allowed to upload the observed layer
//	UMFELD_PEER_OBS_NAME     observed-layer peer (LiDAR / satellite per cell)
//	UMFELD_PEER_OBS_API      its base URL for server-side calls   (…/api/v1 appended)
//	UMFELD_PEER_OBS_WEB      its base URL the browser calls (CORS)  (…/api/v1 appended)
//	UMFELD_PEER_SUB_NAME     subsidy-claims peer (INVEKOS per cell)
//	UMFELD_PEER_SUB_API      its base URL (…/api/v1 appended)
//	UMFELD_PEER_MAP_NAME     the map client that reports declared chunks (docs only)
package main

import (
	"os"
	"strings"
	"sync"
)

type peerConf struct {
	PublicURL, PrivatePrefix string
	ObsName, ObsAPI, ObsWeb  string
	SubName, SubAPI          string
	MapName                  string
}

var (
	peerOnce sync.Once
	peerCfg  peerConf
)

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func peers() peerConf {
	peerOnce.Do(func() {
		trim := func(s string) string { return strings.TrimRight(s, "/") }
		peerCfg = peerConf{
			PublicURL:     trim(envOr("UMFELD_PUBLIC_URL", "")),
			PrivatePrefix: "/" + strings.Trim(envOr("UMFELD_PRIVATE_PREFIX", "/k"), "/"),
			ObsName:       envOr("UMFELD_PEER_OBS_NAME", "observed-peer"),
			ObsAPI:        trim(envOr("UMFELD_PEER_OBS_API", "")),
			ObsWeb:        trim(envOr("UMFELD_PEER_OBS_WEB", envOr("UMFELD_PEER_OBS_API", ""))),
			SubName:       envOr("UMFELD_PEER_SUB_NAME", "subsidy-peer"),
			SubAPI:        trim(envOr("UMFELD_PEER_SUB_API", "")),
			MapName:       envOr("UMFELD_PEER_MAP_NAME", "map-peer"),
		}
	})
	return peerCfg
}

// publicURL — absolute base for links we hand out; "" when not configured
// (callers then emit relative links).
func publicURL() string { return peers().PublicURL }

func publicHost() string {
	u := publicURL()
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if u == "" {
		return "this service"
	}
	return u
}

// peerText — fills the peer placeholders in docs / legal / glyph templates.
func peerText(s string) string {
	if !strings.Contains(s, "{") {
		return s
	}
	p := peers()
	return strings.NewReplacer(
		"{PUBLIC_URL}", p.PublicURL,
		"{PUBLIC_HOST}", publicHost(),
		"{OBS_NAME}", p.ObsName,
		"{OBS_API}", p.ObsAPI,
		"{OBS_WEB}", p.ObsWeb,
		"{SUB_NAME}", p.SubName,
		"{SUB_API}", p.SubAPI,
		"{MAP_NAME}", p.MapName,
	).Replace(s)
}

// setPeersForTest — tests only.
func setPeersForTest(c peerConf) { peerOnce.Do(func() {}); peerCfg = c }
