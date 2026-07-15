package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// MirrorPeers periodically fetches open orders from other uap-indexer
// instances and republishes them into idx. This works without any
// federation protocol or trust between operators because an order is
// self-verifying signed data: PublishOrder re-validates every mirrored
// order against this instance's own chain view (the referenced position
// must be real, unspent, and match the claimed multiplier) exactly as it
// would for a locally-submitted order. A malicious or out-of-date peer
// can only ever offer orders that fail this same validation; it can't
// inject anything a taker's own broadcast wouldn't also reject.
//
// This is one-way (pull) and best-effort: failures fetching or
// republishing an individual peer/order are logged and skipped, never
// fatal. See doc/uap-marketplace-operators-guide.md for the operational
// picture (why you'd want this, what it does and doesn't give you).
func MirrorPeers(idx *Index, peers []string, interval time.Duration) {
	if len(peers) == 0 {
		return
	}
	client := &http.Client{Timeout: 15 * time.Second}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for _, peer := range peers {
			mirrorOnce(client, idx, peer)
		}
		<-ticker.C
	}
}

func mirrorOnce(client *http.Client, idx *Index, peerBase string) {
	url := strings.TrimRight(peerBase, "/") + "/orders"
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("mirror: fetching %s: %v", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("mirror: %s returned HTTP %d", url, resp.StatusCode)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<24))
	if err != nil {
		log.Printf("mirror: reading %s: %v", url, err)
		return
	}
	var orders []Order
	if err := json.Unmarshal(data, &orders); err != nil {
		log.Printf("mirror: decoding %s: %v", url, err)
		return
	}

	added := 0
	for i := range orders {
		o := orders[i] // fresh copy; PublishOrder mutates its argument
		if err := idx.PublishOrder(&o); err != nil {
			// Expected in the common case (already known locally, or not
			// yet visible in our own chain view) as much as for genuine
			// problems -- not worth failing loudly over.
			continue
		}
		added++
	}
	if added > 0 {
		log.Printf("mirror: pulled %d new order(s) from %s", added, peerBase)
	}
}

func parseMirrorPeers(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
