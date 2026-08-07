// Command uap-indexer is a small indexer for Whippet's
// UAP (OP_MINT / OP_MINT_TRANSFER) token positions. It polls a whippetd
// node over RPC, tracks unspent mint/transfer outputs per recipient
// pubkey, and serves them over a tiny HTTP JSON API. It does not require
// any changes to whippetd -- point it at any node's RPC port.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// Configuration comes from two places, and the precedence between them is
// fixed and one-directional:
//
//	explicit command-line flag  >  environment variable  >  built-in default
//
// The mechanism is deliberately boring: the environment is read only to
// compute each flag's *default*, so a flag the operator actually passed
// always wins, and there is no second code path that could disagree with
// the first. An env var set to something unparseable (UAP_RPC_PORT=banana)
// is ignored and the built-in default stands, rather than aborting startup
// over a value the operator can also just override on the command line.
//
// The environment exists mainly for credentials. Anything in argv is world
// readable from /proc/<pid>/cmdline, so `-rpcpassword=hunter2` leaks the
// node's RPC password to every local user; a process's environment is
// readable only by its own user and root. That is what makes systemd's
// EnvironmentFile=/etc/default/uap-indexer (mode 0640, root:uap-indexer)
// the supported way to supply UAP_RPC_USER/UAP_RPC_PASSWORD, and why the
// unit file passes no credential flags at all.

// envOr returns the given environment variable's value, or def if unset.
func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envOrInt is envOr for ints; an unparseable value falls back to def.
func envOrInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envOrBool is envOr for bools, accepting what strconv.ParseBool does
// ("1"/"0", "true"/"false", "yes"/"no" are NOT all accepted -- ParseBool
// takes 1, t, T, TRUE, true, True and their false counterparts).
//
// An unparseable value falls back to def rather than being treated as true.
// That matters for -trustproxy specifically: UAP_TRUSTPROXY=yes must not
// quietly enable header trust, because the operator who typed it would
// have no way to tell it had been misread.
func envOrBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// config is the fully-resolved runtime configuration.
type config struct {
	rpcHost        string
	rpcPort        int
	rpcUser        string
	rpcPass        string
	rpcCookie      string
	startHeight    int64
	pollInterval   time.Duration
	listen         string
	stateFile      string
	reorgWindow    int64
	rateLimit      float64
	rateBurst      float64
	readRateLimit  float64
	readRateBurst  float64
	trustProxy     bool
	readTimeout    time.Duration
	readHdrTimeout time.Duration
	writeTimeout   time.Duration
	idleTimeout    time.Duration
	mirrorPeers    string
	mirrorEvery    time.Duration
}

// parseConfig registers every flag on fs and parses args into a config.
// Split out of main() so the flag/env precedence above is testable without
// starting a server: main() calls it with flag.CommandLine and os.Args[1:].
func parseConfig(fs *flag.FlagSet, args []string) (*config, error) {
	var c config
	fs.StringVar(&c.rpcHost, "rpchost", envOr("UAP_RPC_HOST", "127.0.0.1"), "whippetd RPC host (env UAP_RPC_HOST)")
	fs.IntVar(&c.rpcPort, "rpcport", envOrInt("UAP_RPC_PORT", 33665), "whippetd RPC port, mainnet default (env UAP_RPC_PORT)")
	fs.StringVar(&c.rpcUser, "rpcuser", envOr("UAP_RPC_USER", ""), "whippetd RPC username, ignored if -rpccookiefile is set (env UAP_RPC_USER, preferred: flags are world-readable via /proc)")
	fs.StringVar(&c.rpcPass, "rpcpassword", envOr("UAP_RPC_PASSWORD", ""), "whippetd RPC password, ignored if -rpccookiefile is set (env UAP_RPC_PASSWORD, preferred: flags are world-readable via /proc)")
	fs.StringVar(&c.rpcCookie, "rpccookiefile", envOr("UAP_RPC_COOKIEFILE", ""), "path to whippetd's .cookie file, preferred over -rpcuser/-rpcpassword (env UAP_RPC_COOKIEFILE)")
	fs.Int64Var(&c.startHeight, "startheight", 0, "block height to start indexing from on a fresh state file (e.g. a UAP activation height, to avoid scanning irrelevant history)")
	fs.DurationVar(&c.pollInterval, "pollinterval", 5*time.Second, "how often to poll the node for new blocks")
	fs.StringVar(&c.listen, "listen", envOr("UAP_LISTEN", "127.0.0.1:8961"), "HTTP API listen address (env UAP_LISTEN)")
	fs.StringVar(&c.stateFile, "statefile", envOr("UAP_STATEFILE", "uap-index.sqlite"), "path to the SQLite database holding indexer state, so restarts don't rescan from genesis (env UAP_STATEFILE)")
	fs.Int64Var(&c.reorgWindow, "reorgwindow", DefaultReorgWindow, "how many blocks of undo history to retain; a reorg deeper than this forces a full rebuild (0 retains everything, unbounded)")
	fs.Float64Var(&c.rateLimit, "ratelimit", 30, "max requests per minute per client IP on write endpoints (POST/DELETE /orders); 0 disables rate limiting")
	fs.Float64Var(&c.rateBurst, "rateburst", 10, "extra requests a client may burst immediately before -ratelimit throttling applies")
	fs.Float64Var(&c.readRateLimit, "readratelimit", 300, "max requests per minute per client IP on read endpoints (GET); 0 disables read rate limiting")
	fs.Float64Var(&c.readRateBurst, "readrateburst", 50, "extra requests a client may burst immediately before -readratelimit throttling applies")
	fs.BoolVar(&c.trustProxy, "trustproxy", envOrBool("UAP_TRUSTPROXY", false), "derive the client IP from X-Forwarded-For/X-Real-IP instead of the connecting address (env UAP_TRUSTPROXY). Only set this when a reverse proxy in front of this process overwrites those headers -- otherwise any client can forge its own identity and defeat rate limiting")
	fs.DurationVar(&c.readTimeout, "readtimeout", 15*time.Second, "HTTP request read timeout; prevents slowloris attacks (connection hangs)")
	fs.DurationVar(&c.readHdrTimeout, "readheadertimeout", 5*time.Second, "HTTP request header read timeout; quick defense against slow header transmission")
	fs.DurationVar(&c.writeTimeout, "writetimeout", 15*time.Second, "HTTP response write timeout; prevents slow clients from holding connections")
	fs.DurationVar(&c.idleTimeout, "idletimeout", 30*time.Second, "HTTP idle connection timeout; frees resources held by idle clients")
	fs.StringVar(&c.mirrorPeers, "mirror", "", "comma-separated base URLs of other uap-indexer instances to pull open orders from (e.g. http://peer1:8961,http://peer2:8961), for sharing liquidity across independently-run marketplaces -- see doc/uap-marketplace-operators-guide.md")
	fs.DurationVar(&c.mirrorEvery, "mirrorinterval", 30*time.Second, "how often to poll -mirror peers for new orders")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return &c, nil
}

func main() {
	cfg, err := parseConfig(flag.CommandLine, os.Args[1:])
	if err != nil {
		// flag.CommandLine's default ErrorHandling is ExitOnError, so this
		// is unreachable in practice; handled anyway so a future change of
		// error handling cannot turn into a nil dereference.
		log.Fatalf("parsing flags: %v", err)
	}
	rpc, err := NewRPCClient(cfg.rpcHost, cfg.rpcPort, cfg.rpcUser, cfg.rpcPass, cfg.rpcCookie)
	if err != nil {
		log.Fatalf("rpc client: %v", err)
	}

	store, err := OpenStore(cfg.stateFile)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer store.Close()

	idx, err := store.LoadIndex()
	if err != nil {
		log.Fatalf("loading state from %s: %v", cfg.stateFile, err)
	}
	// Config, not persisted state, so it is applied after loading: the
	// flag the operator passed this run wins over whatever the last run
	// happened to use.
	idx.ReorgWindow = cfg.reorgWindow

	// Nothing else is running yet, but go through the accessor anyway so
	// there is no direct-field-access pattern here for later code to copy.
	s, err := idx.StatusSnapshot()
	if err != nil {
		log.Fatalf("reading state from %s: %v", cfg.stateFile, err)
	}
	if s.TipHeight < 0 {
		log.Printf("no existing state; starting from height %d", cfg.startHeight)
	} else {
		log.Printf("resuming from height %d (%s), %d positions known", s.TipHeight, s.TipHash, s.PositionCount)
	}

	var rl *RateLimiter
	if cfg.rateLimit > 0 {
		rl = NewRateLimiter(cfg.rateLimit, cfg.rateBurst)
		log.Printf("rate limiting write endpoints: %.0f req/min per IP, burst %.0f", cfg.rateLimit, cfg.rateBurst)
	}

	var readRL *RateLimiter
	if cfg.readRateLimit > 0 {
		readRL = NewRateLimiter(cfg.readRateLimit, cfg.readRateBurst)
		log.Printf("rate limiting read endpoints: %.0f req/min per IP, burst %.0f", cfg.readRateLimit, cfg.readRateBurst)
	}

	// Logged unconditionally when on, because the failure mode of enabling
	// this without a proxy actually in front -- every client gets to pick
	// its own rate-limit identity -- is otherwise completely silent.
	if cfg.trustProxy {
		log.Printf("trusting X-Forwarded-For/X-Real-IP for client identity; " +
			"this is only safe if a reverse proxy in front of this process overwrites those headers")
	}

	if peers := parseMirrorPeers(cfg.mirrorPeers); len(peers) > 0 {
		log.Printf("mirroring orders from %d peer(s) every %s: %v", len(peers), cfg.mirrorEvery, peers)
		go MirrorPeers(idx, peers, cfg.mirrorEvery)
	}

	server := &http.Server{
		Addr:              cfg.listen,
		Handler:           newAPIServer(idx, rl, readRL, cfg.trustProxy, rpc),
		ReadTimeout:       cfg.readTimeout,
		ReadHeaderTimeout: cfg.readHdrTimeout,
		WriteTimeout:      cfg.writeTimeout,
		IdleTimeout:       cfg.idleTimeout,
	}
	go func() {
		log.Printf("HTTP API listening on %s", cfg.listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	blocksApplied := 0
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

pollLoop:
	for {
		if err := syncOnce(rpc, idx, cfg.startHeight, &blocksApplied); err != nil {
			log.Printf("sync error: %v", err)
		}

		select {
		case <-ctx.Done():
			break pollLoop
		case <-ticker.C:
		}
	}

	// No final save: every block was committed as it was applied, so
	// there is nothing left in memory that disk does not already have.
	log.Println("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
}

// syncOnce brings the index up to the node's current tip, handling reorgs
// by rolling back to the last common ancestor first.
func syncOnce(rpc *RPCClient, idx *Index, startHeight int64, blocksApplied *int) error {
	nodeHeight, err := rpc.GetBlockCount()
	if err != nil {
		return err
	}

	// Reorg check: walk backward while our recorded hash at a height
	// disagrees with the node's, undoing as we go.
	//
	// The tip is read through idx.Tip() rather than off the struct: the
	// HTTP handlers read TipHeight/TipHash concurrently under the same
	// lock, so touching the fields directly here is a data race. UndoBlock
	// moves the tip itself, atomically with the rest of the rollback.
	for {
		tipHeight, _ := idx.Tip()
		if tipHeight < startHeight {
			break
		}
		nodeHash, err := rpc.GetBlockHash(tipHeight)
		if err != nil {
			return err
		}
		ourHash, ok := idx.HashAtHeight(tipHeight)
		if ok && ourHash == nodeHash {
			break // agreed with the node here; this is the fork point
		}
		if !ok && !idx.IsPruned(tipHeight) {
			break // never indexed this height; nothing to roll back
		}
		// Either the hashes disagree, or the record was pruned and we
		// cannot tell -- and having walked back this far means every
		// height above it disagreed, so a pruned one almost certainly
		// does too. Both need the undo log.
		//
		// The undo log is bounded, so a deep enough reorg reaches a
		// height we can no longer reverse. UndoBlock is a no-op there
		// and does not move the tip, so without this check the loop
		// would spin forever. Rebuilding is the only way back to a
		// coherent view: the index holds state from the orphaned chain
		// and has no record of how to remove it.
		if !idx.CanUndo(tipHeight) {
			log.Printf("reorg at height %d is deeper than the %d-block undo window; "+
				"discarding the index and rebuilding from height %d",
				tipHeight, idx.ReorgWindow, startHeight)
			idx.Reset()
			break
		}
		log.Printf("reorg detected at height %d (indexed %s, node has %s); rolling back", tipHeight, ourHash, nodeHash)
		idx.UndoBlock(tipHeight)
	}

	tipHeight, _ := idx.Tip()
	next := tipHeight + 1
	if tipHeight < 0 {
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
		*blocksApplied++

		// A store failure latches, so every block after the first one is
		// applied in memory but not persisted. Stop and report rather
		// than racing ahead building state that will be lost on restart.
		if err := idx.StoreErr(); err != nil {
			return fmt.Errorf("persisting block %d: %w", block.Height, err)
		}
	}
	return nil
}
