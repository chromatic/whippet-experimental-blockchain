// Command uap-indexer is a small, dependency-free indexer for Whippet's
// UAP (OP_MINT / OP_MINT_TRANSFER) token positions. It polls a whippetd
// node over RPC, tracks unspent mint/transfer outputs per recipient
// pubkey, and serves them over a tiny HTTP JSON API. It does not require
// any changes to whippetd -- point it at any node's RPC port.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// envOr returns the given environment variable's value, or def if unset.
// Used so RPC credentials can be supplied via environment (e.g. Docker
// secrets/env files) instead of command-line flags, which show up in
// `docker inspect`/process listings far more readily than env vars do.
// An explicit flag always overrides the environment default.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	var (
		rpcHost      = flag.String("rpchost", envOr("UAP_RPC_HOST", "127.0.0.1"), "whippetd RPC host (env UAP_RPC_HOST)")
		rpcPort      = flag.Int("rpcport", envOrInt("UAP_RPC_PORT", 33665), "whippetd RPC port, mainnet default (env UAP_RPC_PORT)")
		rpcUser      = flag.String("rpcuser", envOr("UAP_RPC_USER", ""), "whippetd RPC username, ignored if -rpccookiefile is set (env UAP_RPC_USER)")
		rpcPass      = flag.String("rpcpassword", envOr("UAP_RPC_PASSWORD", ""), "whippetd RPC password, ignored if -rpccookiefile is set (env UAP_RPC_PASSWORD)")
		rpcCookie    = flag.String("rpccookiefile", envOr("UAP_RPC_COOKIEFILE", ""), "path to whippetd's .cookie file, preferred over -rpcuser/-rpcpassword (env UAP_RPC_COOKIEFILE)")
		startHeight  = flag.Int64("startheight", 0, "block height to start indexing from on a fresh state file (e.g. a UAP activation height, to avoid scanning irrelevant history)")
		pollInterval = flag.Duration("pollinterval", 5*time.Second, "how often to poll the node for new blocks")
		listen       = flag.String("listen", envOr("UAP_LISTEN", "127.0.0.1:8961"), "HTTP API listen address (env UAP_LISTEN)")
		stateFile    = flag.String("statefile", envOr("UAP_STATEFILE", "uap-index.json"), "path to persist indexer state, so restarts don't rescan from genesis (env UAP_STATEFILE)")
		saveEvery    = flag.Int("saveevery", 20, "save state to disk every N blocks indexed")
		rateLimit    = flag.Float64("ratelimit", 30, "max requests per minute per client IP on write endpoints (POST/DELETE /orders); 0 disables rate limiting")
		rateBurst    = flag.Float64("rateburst", 10, "extra requests a client may burst immediately before -ratelimit throttling applies")
		trustProxy   = flag.Bool("trustproxy", false, "trust the X-Forwarded-For header for rate-limiting client identity (only if genuinely deployed behind a reverse proxy that sets it -- otherwise this lets any client bypass the limiter)")
		mirrorPeers  = flag.String("mirror", "", "comma-separated base URLs of other uap-indexer instances to pull open orders from (e.g. http://peer1:8961,http://peer2:8961), for sharing liquidity across independently-run marketplaces -- see doc/uap-marketplace-operators-guide.md")
		mirrorEvery  = flag.Duration("mirrorinterval", 30*time.Second, "how often to poll -mirror peers for new orders")
	)
	flag.Parse()

	rpc, err := NewRPCClient(*rpcHost, *rpcPort, *rpcUser, *rpcPass, *rpcCookie)
	if err != nil {
		log.Fatalf("rpc client: %v", err)
	}

	idx, err := LoadIndex(*stateFile)
	if err != nil {
		log.Fatalf("loading state file %s: %v", *stateFile, err)
	}
	if idx.TipHeight < 0 {
		log.Printf("no existing state; starting from height %d", *startHeight)
	} else {
		log.Printf("resuming from height %d (%s), %d positions known", idx.TipHeight, idx.TipHash, len(idx.Positions))
	}

	var rl *RateLimiter
	if *rateLimit > 0 {
		rl = NewRateLimiter(*rateLimit, *rateBurst)
		log.Printf("rate limiting write endpoints: %.0f req/min per IP, burst %.0f", *rateLimit, *rateBurst)
	}

	if peers := parseMirrorPeers(*mirrorPeers); len(peers) > 0 {
		log.Printf("mirroring orders from %d peer(s) every %s: %v", len(peers), *mirrorEvery, peers)
		go MirrorPeers(idx, peers, *mirrorEvery)
	}

	server := &http.Server{Addr: *listen, Handler: newAPIServer(idx, rl, *trustProxy)}
	go func() {
		log.Printf("HTTP API listening on %s", *listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	blocksSinceSave := 0
	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()

pollLoop:
	for {
		if err := syncOnce(rpc, idx, *startHeight, &blocksSinceSave, *saveEvery, *stateFile); err != nil {
			log.Printf("sync error: %v", err)
		}

		select {
		case <-ctx.Done():
			break pollLoop
		case <-ticker.C:
		}
	}

	log.Println("shutting down, saving final state...")
	if err := idx.Save(*stateFile); err != nil {
		log.Printf("final save failed: %v", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

// syncOnce brings the index up to the node's current tip, handling reorgs
// by rolling back to the last common ancestor first.
func syncOnce(rpc *RPCClient, idx *Index, startHeight int64, blocksSinceSave *int, saveEvery int, stateFile string) error {
	nodeHeight, err := rpc.GetBlockCount()
	if err != nil {
		return err
	}

	// Reorg check: walk backward while our recorded hash at a height
	// disagrees with the node's, undoing as we go.
	for idx.TipHeight >= startHeight {
		nodeHash, err := rpc.GetBlockHash(idx.TipHeight)
		if err != nil {
			return err
		}
		ourHash, ok := idx.HashAtHeight(idx.TipHeight)
		if !ok || ourHash == nodeHash {
			break
		}
		log.Printf("reorg detected at height %d (indexed %s, node has %s); rolling back", idx.TipHeight, ourHash, nodeHash)
		idx.UndoBlock(idx.TipHeight)
		idx.TipHeight--
	}

	next := idx.TipHeight + 1
	if idx.TipHeight < 0 {
		next = startHeight
	}

	for next <= nodeHeight {
		hash, err := rpc.GetBlockHash(next)
		if err != nil {
			return err
		}
		block, err := rpc.GetBlockVerbose(hash)
		if err != nil {
			return err
		}
		idx.ApplyBlock(block)
		next++
		*blocksSinceSave++

		if *blocksSinceSave >= saveEvery {
			if err := idx.Save(stateFile); err != nil {
				log.Printf("periodic save failed: %v", err)
			}
			*blocksSinceSave = 0
		}
	}
	return nil
}
