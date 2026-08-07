# Whippetcoin Faucet

A simple web faucet for Whippetcoin, written in Go.

## Features
- Configurable coins per claim (via SQLite config)
- Address validation
- Per-IP, user agent, and cookie cooldown (24h)
- Daily claim limit (configurable)
- Simple HTML/CSS UI
- Talks to `whippetd` over JSON-RPC (no `whippet-cli` fork per request)

## Usage

1. Build the faucet:

    make

2. Run the faucet, pointing it at your node's RPC:

    export FAUCET_PORT=8080
    ./faucet -rpc-cookie ~/.whippet/mainnet/.cookie

   Or with an explicit user/password from your `whippet.conf`:

    ./faucet -rpc-host 127.0.0.1 -rpc-port 33665 \
             -rpc-user <user> -rpc-pass <password>

   Behind a reverse proxy that rewrites `X-Forwarded-For`, add
   `-trustproxy`. Do NOT set it otherwise: without a proxy actively
   rewriting the header, any client can spoof its IP and bypass the
   per-IP cooldown.

3. Point your browser (or nginx proxy) to the configured port.

4. The faucet will create and use `faucet.sqlite` in the current directory.

## Configuration

- Edit the `config` table in `faucet.sqlite` to change:
    - `faucet_amount`: coins per claim
    - `daily_claim_limit`: max claims per UTC day

## RPC flags

| flag | default | meaning |
| --- | --- | --- |
| `-rpc-host` | `127.0.0.1` | whippetd RPC host |
| `-rpc-port` | `33665` | whippetd RPC port (mainnet) |
| `-rpc-user` / `-rpc-pass` | empty | credentials from `whippet.conf` |
| `-rpc-cookie` | empty | path to the node's `.cookie`; overrides user/pass |
| `-trustproxy` | `false` | honour `X-Forwarded-For` (see above) |

## Testing

    go test ./...

The node is reached through a `NodeClient` interface, so the whole HTTP
path is exercised against a fake that records payments instead of making
them. **The tests never send coins and never need a running node** —
that is the point of the interface. `NewFaucetHandler` is the single
definition of the handler; `main()` and the tests both go through it, so
a test cannot pass against a copy that production does not run.

## Requirements
- Go 1.18+
- A reachable `whippetd` RPC endpoint with a wallet holding funds

## License
MIT

## Project
https://github.com/chromatic/whippet-experimental-blockchain
