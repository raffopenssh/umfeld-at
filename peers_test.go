package main

// Tests run against a fixed, fictitious peer configuration so that no real
// host name is baked into the test corpus.
func init() {
	setPeersForTest(peerConf{
		PublicURL: "https://umfeld.example", PrivatePrefix: "/k",
		ObsName: "obs-peer", ObsAPI: "https://obs.example:8100", ObsWeb: "https://obs.example",
		SubName: "sub-peer", SubAPI: "https://sub.example",
		MapName: "map-peer",
	})
}
