package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// clientIP extracts the connecting client IP from an HTTP request.
// When trustProxy is false (default), it uses only r.RemoteAddr and ignores
// any X-Forwarded-For header. This is safe against spoofing but doesn't work
// behind a reverse proxy where all connections appear from the proxy's address.
//
// When trustProxy is true, it honors the X-Forwarded-For header (falling back
// to r.RemoteAddr if absent), extracting the LAST entry from a comma-separated
// list. This is the entry appended by your own trusted reverse proxy, not
// client-controlled. CRITICAL ASSUMPTION: This implementation is correct only
// for exactly one trusted proxy hop. If you have N chained trusted proxies,
// you would need to skip N-1 entries from the right. Blindly trusting any
// entry further left than that is attacker-controlled and defeats the purpose
// of this flag. See the warning in the -trustproxy flag docs.
//
// In all cases, the port is stripped from the result. Handles IPv4 (1.2.3.4:5678 -> 1.2.3.4),
// IPv6 ([::1]:1234 -> ::1), and addresses without ports.
func clientIP(r *http.Request, trustProxy bool) string {
	var addr string

	if trustProxy {
		// Honor X-Forwarded-For if present (fallback to RemoteAddr if absent)
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// X-Forwarded-For can be a comma-separated list: "attacker, proxy1, real_client".
			// With a trusted reverse proxy using proxy_add_x_forwarded_for,
			// the rightmost entry is the one our proxy appended (the real client IP).
			// Entries to the left are either client-provided (attacker-controlled)
			// or from intermediate proxies. We extract the last non-empty entry.
			// If the header ends with a comma (e.g., "1.1.1.1, "), it indicates
			// an incomplete header: the proxy was supposed to append but didn't.
			// In that case, we reject it and fall back to RemoteAddr.
			xffTrimmed := strings.TrimSpace(xff)
			if strings.HasSuffix(xffTrimmed, ",") {
				// Incomplete header: proxy didn't append as expected. Fall back to RemoteAddr.
				addr = r.RemoteAddr
			} else {
				entries := strings.Split(xff, ",")
				// Find the rightmost non-empty entry
				for i := len(entries) - 1; i >= 0; i-- {
					if trimmed := strings.TrimSpace(entries[i]); trimmed != "" {
						addr = trimmed
						break
					}
				}
				// If all entries were empty/whitespace, fall back to RemoteAddr
				if addr == "" {
					addr = r.RemoteAddr
				}
			}
		} else {
			addr = r.RemoteAddr
		}
	} else {
		// Default: use only RemoteAddr, ignore X-Forwarded-For entirely.
		addr = r.RemoteAddr
	}

	// Strip port from the address (handles IPv4, IPv6, and no-port inputs)
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Fallback: if SplitHostPort fails, return addr as-is
		// (some addresses may not have a port, including bare IPv6)
		return addr
	}
	return host
}

func main() {
	trustProxy := flag.Bool("trustproxy", false,
		"Honor X-Forwarded-For header for client IP extraction. "+
			"SECURITY: Only enable when deployed behind a trusted reverse proxy "+
			"that actively rewrites this header. If enabled and the proxy does not "+
			"rewrite X-Forwarded-For, any client can spoof their IP by setting the header.")
	rpcHost := flag.String("rpc-host", "127.0.0.1", "whippetd RPC host")
	rpcPort := flag.Int("rpc-port", 33665, "whippetd RPC port")
	rpcUser := flag.String("rpc-user", "", "whippetd RPC user (from config)")
	rpcPass := flag.String("rpc-pass", "", "whippetd RPC password (from config)")
	rpcCookie := flag.String("rpc-cookie", "", "whippetd RPC cookie file path (~/.whippet/mainnet/.cookie)")
	flag.Parse()

	// Create RPC client from config
	client, err := NewRPCClient(*rpcHost, *rpcPort, *rpcUser, *rpcPass, *rpcCookie)
	if err != nil {
		log.Fatalf("Failed to create RPC client: %v", err)
	}

	db, err := sql.Open("sqlite3", "./faucet.sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	initDB(db)

	http.Handle("/", NewFaucetHandler(db, client, *trustProxy))

	port := os.Getenv("FAUCET_PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Faucet running on :%s", port)
	http.ListenAndServe(":"+port, nil)
}

// NodeClient is the faucet's view of the Whippet node. Everything that
// touches the chain goes through this interface, so the HTTP layer can be
// exercised in tests with no node running and no coins at risk.
type NodeClient interface {
	ValidateAddress(address string) (bool, error)
	SendToAddress(address string, amount float64) (txid string, err error)
}

// NewFaucetHandler builds the faucet's complete HTTP surface. main() and the
// tests both go through here: a test that drove its own copy of these
// handlers would prove nothing about what actually runs in production.
func NewFaucetHandler(db *sql.DB, client NodeClient, trustProxy bool) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write([]byte(`body { background: #f7fafc; font-family: sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; }
		       .faucet-box { background: #fff; padding: 2em 2.5em; border-radius: 12px; box-shadow: 0 2px 16px #0001; min-width: 320px; }
		       h1 { color: #3b5998; margin-bottom: 0.5em; }
		       label { display: block; margin-bottom: 0.5em; font-weight: bold; }
		       input[type="text"] { width: 100%; padding: 0.5em; border: 1px solid #ccc; border-radius: 6px; margin-bottom: 1em; font-size: 1em; }
		       button { background: #3b5998; color: #fff; border: none; padding: 0.7em 1.5em; border-radius: 6px; font-size: 1em; cursor: pointer; transition: background 0.2s; }
		       button:hover { background: #29487d; }
		       .footer { margin-top: 1.5em; color: #888; font-size: 0.9em; text-align: center; }
		       .footer a { color: #3b5998; text-decoration: none; }
		       .footer a:hover { text-decoration: underline; }`))
	})

	renderPage := func(content string) string {
		return `<!DOCTYPE html>
		       <html lang="en">
		       <head>
		       <meta charset="UTF-8">
		       <meta name="viewport" content="width=device-width, initial-scale=1.0">
		       <title>Whippetcoin Faucet</title>
		       <link rel="stylesheet" href="/style.css">
		       </head>
		       <body>
		       <div class="faucet-box">
		       ` + content + `
		       <div class="footer">One request per 24 hours. Powered by <a href="https://github.com/chromatic/whippet-experimental-blockchain" target="_blank">Whippetcoin</a>.</div>
		       </div>
		       </body>
		       </html>`
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			form := `<h1>Whippetcoin Faucet</h1>
			       <form method="POST">
				       <label for="address">Your Whippetcoin Address</label>
				       <input type="text" id="address" name="address" placeholder="Enter your address" required>
				       <button type="submit">Get Coins</button>
			       </form>`
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(renderPage(form)))
			return
		}
		if r.Method == "POST" {
			address := r.FormValue("address")

			// Validate address using NodeClient
			isValid, err := client.ValidateAddress(address)
			if err != nil {
				log.Printf("validateaddress error: %v", err)
				http.Error(w, "Node error checking address", http.StatusInternalServerError)
				return
			}
			if !isValid {
				log.Printf("address %s is invalid", address)
				http.Error(w, "Invalid address", http.StatusBadRequest)
				return
			}

			ip := clientIP(r, trustProxy)
			ua := r.UserAgent()
			cookie, err := r.Cookie("faucet_id")
			var faucetID string
			if err == nil {
				faucetID = cookie.Value
			} else {
				// No cookie present: generate one for future use, but do NOT use a
				// unique ID for this request's rate-limit check. Using an empty string
				// ensures the IP-based rate limit applies even without a cookie,
				// preventing attackers from bypassing the limit by omitting cookies.
				faucetID = ""
				newCookieValue := fmt.Sprintf("%d", time.Now().UnixNano())
				http.SetCookie(w, &http.Cookie{Name: "faucet_id", Value: newCookieValue, Path: "/", Expires: time.Now().Add(365 * 24 * time.Hour)})
			}
			if !canRequest(db, ip, ua, faucetID) {
				http.Error(w, "You can only request coins once every 24 hours.", http.StatusTooManyRequests)
				return
			}

			// Check daily claim limit
			dailyLimit, err := getDailyClaimLimit(db)
			if err != nil {
				http.Error(w, "Daily claim limit not configured.", http.StatusInternalServerError)
				return
			}
			today := time.Now().UTC()
			startOfDay := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).Unix()
			claimsToday, err := getClaimsToday(db, startOfDay)
			if err != nil {
				http.Error(w, "Failed to check daily claims.", http.StatusInternalServerError)
				return
			}
			if claimsToday >= dailyLimit {
				http.Error(w, "Faucet daily claim limit reached. Try again after midnight UTC.", http.StatusTooManyRequests)
				return
			}

			faucetAmount, err := getFaucetAmount(db)
			if err != nil {
				http.Error(w, "Faucet amount not configured.", http.StatusInternalServerError)
				return
			}

			// Send coins using NodeClient
			log.Printf("Sending %f coins to address %s", faucetAmount, address)
			txid, err := client.SendToAddress(address, faucetAmount)
			if err != nil {
				log.Printf("sendtoaddress error: %v", err)
				http.Error(w, "Error sending coins", http.StatusInternalServerError)
				return
			}
			log.Printf("sendtoaddress result: %s", txid)

			// Log the successful request only after successful send
			logRequest(db, ip, ua, faucetID, address)

			result := `<h1>Whippetcoin Faucet</h1>
			<div style="margin-bottom:1em;">Coins sent!</div>
			<pre style="background:#f7fafc;border-radius:6px;padding:1em;">` + txid + `</pre>
			<a href="/">Back to faucet</a>`
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(renderPage(result)))
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	return mux
}

func initDB(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS requests (
		id INTEGER PRIMARY KEY,
		ip TEXT,
		ua TEXT,
		cookie TEXT,
		address TEXT,
		timestamp INTEGER
	)`)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT
	)`)
	if err != nil {
		log.Fatal(err)
	}
	// Insert default faucet amount if not set
	_, err = db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES ('faucet_amount', '10.0')`)
	if err != nil {
		log.Fatal(err)
	}
	// Insert default daily claim limit if not set
	_, err = db.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES ('daily_claim_limit', '100')`)
	if err != nil {
		log.Fatal(err)
	}
}

func getFaucetAmount(db *sql.DB) (float64, error) {
	row := db.QueryRow(`SELECT value FROM config WHERE key='faucet_amount'`)
	var val string
	if err := row.Scan(&val); err != nil {
		return 0, err
	}
	return strconv.ParseFloat(val, 64)
}

func getDailyClaimLimit(db *sql.DB) (int, error) {
	row := db.QueryRow(`SELECT value FROM config WHERE key='daily_claim_limit'`)
	var val string
	if err := row.Scan(&val); err != nil {
		return 0, err
	}
	return strconv.Atoi(val)
}

func getClaimsToday(db *sql.DB, startOfDay int64) (int, error) {
	row := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE timestamp >= ?`, startOfDay)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func canRequest(db *sql.DB, ip, ua, cookie string) bool {
	// Check if this client has made a request within the past 24 hours.
	// We block on IP match (primary identifier) or cookie match (persistent ID).
	// User-Agent is fully client-controlled and is a weak signal retained only
	// as defence in depth; we don't block solely on UA match.

	cutoff := time.Now().Add(-24 * time.Hour).Unix()

	// First, check if IP has made a recent request
	row := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE ip=? AND timestamp>?`, ip, cutoff)
	var count int
	if err := row.Scan(&count); err != nil {
		return false
	}
	if count > 0 {
		return false // IP match found, deny
	}

	// If cookie is present (non-empty), check for cookie match
	if cookie != "" {
		row = db.QueryRow(`SELECT COUNT(*) FROM requests WHERE cookie=? AND timestamp>?`, cookie, cutoff)
		if err := row.Scan(&count); err != nil {
			return false
		}
		if count > 0 {
			return false // Cookie match found, deny
		}
	}

	return true // No match, allow
}

func logRequest(db *sql.DB, ip, ua, cookie, address string) {
	_, err := db.Exec(`INSERT INTO requests (ip, ua, cookie, address, timestamp) VALUES (?, ?, ?, ?, ?)`, ip, ua, cookie, address, time.Now().Unix())
	if err != nil {
		log.Println("Failed to log request:", err)
	}
}
