<h1 align="center">
<img src="share/pixmaps/whippet256.svg" alt="Whippet" width="256"/>
<br/><br/>
Whippet Core [WHT, Ð]
</h1>

For internationalized documentation, see the index at [doc/intl](doc/intl/README.md).

Whippet is a fork of Dogecoin intended to experiment with code and network
changes around blocktime. It is not intended for anything else. The Whippet
Core software allows anyone to operate a node in the Whippet blockchain
networks and uses the Scrypt hashing method for Proof of Work. It is adapted
from Bitcoin Core and other cryptocurrencies.

### Experimental fork

This codebase is an experiment for a small, non-commercial network. It is
**not** intended for exchange listings, token sales, or speculative trading.
Use it if you want to explore the protocol, run a node, or mine on the
experimental network; do not treat it as production or investment software.

## Build and run

**Docker (recommended):**

```
docker build -f Dockerfile.ubuntu24.04 -t whippet-ubuntu24 .
docker run --rm -u $(id -u):$(id -g) -v "$PWD:/src" -v /tmp/whippet-ccache:/root/.ccache -v /tmp/whippet-build:/build whippet-ubuntu24
ls -lh /tmp/whippet-build/whippetd /tmp/whippet-build/whippet-cli
```

**Native build (Linux):**

```
./autogen.sh
./configure
make -j$(nproc)
```

**Run a node (mainnet defaults):**

```
./src/whippetd -daemon
./src/whippet-cli getblockchaininfo
```

The JSON-RPC API is self-documenting and can be browsed with `whippet-cli help`, while detailed information for each command can be viewed with `whippet-cli help <command>`.

## Mining

To contribute blocks to the network, see [contrib/mining](contrib/mining/) for mining tools and instructions.

### Such ports

Whippet Core by default uses port `33666` for peer-to-peer communication that
is needed to synchronize the "mainnet" blockchain and stay informed of new
transactions and blocks. Additionally, a JSONRPC port can be opened, which
defaults to port `33665` for mainnet nodes. It is strongly recommended to not
expose RPC ports to the public internet.

| Function | mainnet | testnet | regtest |
| :------- | ------: | ------: | ------: |
| P2P      |   33666 |   44556 |   18444 |
| RPC      |   33665 |   44555 |   18332 |

## Ongoing development

This is an experiment. There are no plans to make this anything else. This code
may change. There may be no consensus. There may be forks. The network may be
offline for long periods of time, perhaps for good.

## Very Much Frequently Asked Questions

What's the point? Three things:

- to test a blocktime of 6 seconds (this is 10x faster than Dogecoin)
- to test a minimum block time consensus rule (blocks must be at least 6
  seconds apart)
- to evaluate merge mining risks with a 6 second blocktime

Do you have a question regarding Whippet? An answer is perhaps already in the
[FAQ](doc/FAQ.md) or the
[Q&A section](https://github.com/whippet/whippet/discussions/categories/q-a)
of the discussion board!

## License
Whippet Core is released under the terms of the MIT license. See
[COPYING](COPYING) for more information.
