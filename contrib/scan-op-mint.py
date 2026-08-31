#!/usr/bin/env python3
"""Count OP_MINT / OP_MINT_TRANSFER outputs on chain.

Answers one question: are there any real UAP positions out there? If the
answer is zero, the covenant format can still be changed without stranding
anyone's coins.

Read-only. The RPC allowlist below is the whole safety story: this script
cannot send, sign, or import anything, because it cannot name a method that
would. It writes nothing and starts no daemon.

Credentials come from ~/.whippet/whippet.conf by default, or the environment,
so the password need never appear in ps output or shell history.

    ./scan-op-mint.py                          # mainnet, from the activation height
    ./scan-op-mint.py --start 0                # paranoid: scan from genesis
    ./scan-op-mint.py --conf /path/to/whippet.conf
"""

import argparse
import base64
import json
import os
import sys
import urllib.request

# Height below which OP_MINT/OP_MINT_TRANSFER are rejected as undefined, so
# no position can exist before it.
#
# NOTE: chainparams.cpp:90 in this tree says 80000. The deployed activation
# was 65000, so that is what this defaults to -- starting too low only costs
# scanning time, while starting too high silently misses every position in
# between and would report a reassuring zero. If the two are genuinely out of
# step, the source is the one that needs fixing.
UAP_MINT_HEIGHT = 65000

OP_MINT = 0xB5
OP_MINT_TRANSFER = 0xBA

ALLOWED_METHODS = {"getblockcount", "getblockhash", "getblock"}


def rpc(url, auth, method, params):
    if method not in ALLOWED_METHODS:
        raise SystemExit(f"refusing to call non-read-only method {method!r}")
    body = json.dumps({"jsonrpc": "1.0", "id": "scan", "method": method,
                       "params": params}).encode()
    req = urllib.request.Request(url, data=body)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Basic " + base64.b64encode(auth.encode()).decode())
    with urllib.request.urlopen(req, timeout=120) as resp:
        payload = json.load(resp)
    if payload.get("error"):
        raise SystemExit(f"RPC error on {method}: {payload['error']}")
    return payload["result"]


def resolve_auth(args):
    """Find RPC credentials, preferring the places that don't leak them.

    Order: explicit flags, environment, the config file, then a cookie. The
    config file is the default because the credentials already live there --
    a password passed as --rpc-pass is visible to every other user on the box
    via ps, and lands in shell history besides. The password is never printed
    or echoed, including in error messages.
    """
    if args.rpc_user and args.rpc_pass:
        return f"{args.rpc_user}:{args.rpc_pass}"

    env_user = os.environ.get("WHIPPET_RPC_USER")
    env_pass = os.environ.get("WHIPPET_RPC_PASS")
    if env_user and env_pass:
        return f"{env_user}:{env_pass}"

    conf_path = os.path.expanduser(args.conf)
    if os.path.exists(conf_path):
        found = {}
        with open(conf_path) as fh:
            for line in fh:
                line = line.strip()
                if line.startswith("#") or "=" not in line:
                    continue
                key, _, value = line.partition("=")
                key = key.strip().lower()
                if key in ("rpcuser", "rpcpassword"):
                    found[key] = value.strip()
        if "rpcuser" in found and "rpcpassword" in found:
            return f"{found['rpcuser']}:{found['rpcpassword']}"

    cookie_path = os.path.expanduser(args.cookie)
    if os.path.exists(cookie_path):
        with open(cookie_path) as fh:
            return fh.read().strip()

    raise SystemExit(
        "no RPC credentials found. Tried --rpc-user/--rpc-pass, then\n"
        "  WHIPPET_RPC_USER / WHIPPET_RPC_PASS in the environment, then\n"
        f"  rpcuser/rpcpassword in {conf_path}, then\n"
        f"  a cookie at {cookie_path}.\n"
        "Set the env vars to keep the password out of ps and shell history:\n"
        "  read -rs WHIPPET_RPC_PASS && export WHIPPET_RPC_PASS"
    )


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rpc-host", default="127.0.0.1")
    ap.add_argument("--rpc-port", type=int, default=33665)
    ap.add_argument("--conf", default="~/.whippet/whippet.conf",
                    help="config file to read rpcuser/rpcpassword from")
    ap.add_argument("--rpc-user")
    ap.add_argument("--rpc-pass",
                    help="avoid: visible in ps and shell history, prefer --conf or the env vars")
    ap.add_argument("--cookie", default="~/.whippet/mainnet/.cookie")
    ap.add_argument("--start", type=int, default=UAP_MINT_HEIGHT)
    args = ap.parse_args()

    auth = resolve_auth(args)
    url = f"http://{args.rpc_host}:{args.rpc_port}/"
    tip = rpc(url, auth, "getblockcount", [])
    start = max(0, args.start)
    print(f"scanning heights {start}..{tip} ({tip - start + 1} blocks)", file=sys.stderr)

    mints, transfers, examples = 0, 0, []

    for height in range(start, tip + 1):
        if height % 2000 == 0:
            print(f"  ...{height}", file=sys.stderr)
        blockhash = rpc(url, auth, "getblockhash", [height])

        # Cheap prefilter: pull the raw block and look for the opcode bytes.
        # Any real UAP output contains one, so a block without either byte
        # anywhere cannot hold a position. False positives are fine -- they
        # just cost one verbose fetch -- and this keeps the expensive
        # verbosity-2 decode off the overwhelming majority of blocks.
        raw = bytes.fromhex(rpc(url, auth, "getblock", [blockhash, 0]))
        if OP_MINT not in raw and OP_MINT_TRANSFER not in raw:
            continue

        block = rpc(url, auth, "getblock", [blockhash, 2])
        for tx in block.get("tx", []):
            for vout in tx.get("vout", []):
                asm = vout.get("scriptPubKey", {}).get("asm", "")
                tokens = asm.split()
                is_mint = "OP_MINT" in tokens
                is_transfer = "OP_MINT_TRANSFER" in tokens
                if not (is_mint or is_transfer):
                    continue
                if is_mint:
                    mints += 1
                else:
                    transfers += 1
                if len(examples) < 10:
                    examples.append(
                        f"    height {height}  {tx['txid']}:{vout['n']}  "
                        f"{'MINT' if is_mint else 'TRANSFER'}  {vout.get('value')}"
                    )

    print()
    print(f"OP_MINT outputs:          {mints}")
    print(f"OP_MINT_TRANSFER outputs: {transfers}")
    if examples:
        print("\nfirst few:")
        print("\n".join(examples))
    else:
        print("\nnothing on chain -- the covenant format is not load-bearing yet.")


if __name__ == "__main__":
    main()
