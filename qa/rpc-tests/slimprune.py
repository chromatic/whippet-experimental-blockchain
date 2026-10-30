#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

#
# Pruning with the slim storage options (-slimblocks -slimprevprefix -slimundo -slimblockindex).
# Node 0 prunes automatically and writes slim records; node 1 keeps everything in the legacy format.
# After block files have been pruned, node 0 must
# - still serve the blocks it kept, byte for byte as node 1 does, and refuse the pruned ones,
# - keep slim prefix-only records in the files that are left,
# - restart on the pruned data directory, and
# - reorg through slim records with short undo checksums.
# Uses -fastprune, so the "big" blocks are 1/50 of the real size (see pruning.py).
#

import os
import struct

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *
from pruning import mine_large_blocks, calc_usage, PRUNE_MIB, SYNC_TIMEOUT

REGTEST_MAGIC = bytes.fromhex('fabfb5da')
SLIM_FLAG = 0x80000000
FLAG_PREV_PREFIX = 1 << 3

SLIM_ARGS = ["-slimblocks", "-slimprevprefix", "-slimundo", "-slimblockindex"]


def records(path):
    data = open(path, 'rb').read()
    pos, recs = 0, []
    while data[pos:pos + 4] == REGTEST_MAGIC:
        word = struct.unpack_from('<I', data, pos + 4)[0]
        size = word & 0x7fffffff
        recs.append((bool(word & SLIM_FLAG), size > 0 and bool(data[pos + 8] & FLAG_PREV_PREFIX) and bool(word & SLIM_FLAG)))
        pos += 8 + size
    return recs


class SlimPruneTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.num_nodes = 2
        self.setup_clean_chain = True

    def node0_args(self):
        return ["-debug", "-fastprune", "-maxreceivebuffer=20000", "-prune=%d" % PRUNE_MIB] + SLIM_ARGS

    def setup_network(self, split=False):
        self.nodes = [
            start_node(0, self.options.tmpdir, self.node0_args(), timewait=1200),
            start_node(1, self.options.tmpdir, ["-debug", "-fastprune", "-maxreceivebuffer=20000"], timewait=1200),
        ]
        connect_nodes_bi(self.nodes, 0, 1)
        self.is_network_split = False
        self.blocksdir = os.path.join(self.options.tmpdir, "node0", "regtest", "blocks")

    def blk_files(self):
        return sorted(f for f in os.listdir(self.blocksdir) if f.startswith("blk") and f.endswith(".dat"))

    def mixed_blocks(self, n):
        """Blocks with spends (so with undo data) and without."""
        addr = self.nodes[0].getnewaddress()
        for _ in range(n):
            self.nodes[1].sendtoaddress(addr, 1)
            self.nodes[1].sendtoaddress(addr, 2)
            self.nodes[1].generate(1)
            self.nodes[1].generate(1)

    def run_test(self):
        a, b = self.nodes
        b.generate(130)                 # coins to spend, mature
        sync_blocks(self.nodes)
        self.mixed_blocks(20)
        sync_blocks(self.nodes)

        # Enough big blocks to pass the prune target and the 1440 block window.
        mine_large_blocks(b, 2700)
        sync_blocks(self.nodes, timeout=SYNC_TIMEOUT)
        self.mixed_blocks(10)
        sync_blocks(self.nodes, timeout=SYNC_TIMEOUT)
        tip = b.getblockcount()
        assert_equal(a.getblockcount(), tip)
        assert_equal(a.getbestblockhash(), b.getbestblockhash())

        files = self.blk_files()
        assert "blk00000.dat" not in files, "the first block file should have been pruned: %s" % files
        usage = calc_usage(self.blocksdir + "/")
        print("block files left: %s, usage %.1f MiB (target %d)" % (len(files), usage, PRUNE_MIB))
        assert usage <= PRUNE_MIB

        # Pruned blocks are refused, kept ones are the same bytes as on the legacy node.
        assert_raises_jsonrpc(-1, "Block not available (pruned data)", a.getblock, a.getblockhash(5))
        for h in range(tip - 200, tip + 1):
            hash_ = b.getblockhash(h)
            assert_equal(a.getblock(hash_, False), b.getblock(hash_, False))
        print("blocks kept by the pruned slim node read back identical to the legacy node")

        # The files that are left hold slim records with only a prefix of the parent hash.
        recs = []
        for f in files:
            recs += records(os.path.join(self.blocksdir, f))
        assert len(recs) > 0
        assert all(slim for slim, _ in recs), "legacy record in a slim node's files"
        assert all(prefix for _, prefix in recs)
        print("%d records in the remaining files, all slim with a prefix of the parent hash" % len(recs))

        # Restart on the pruned data directory.
        best = a.getbestblockhash()
        stop_node(a, 0)
        self.nodes[0] = start_node(0, self.options.tmpdir, self.node0_args(), timewait=1200)
        a = self.nodes[0]
        connect_nodes_bi(self.nodes, 0, 1)
        assert_equal(a.getbestblockhash(), best)
        assert a.verifychain(3, 100)
        print("restart on the pruned data directory: same tip, verifychain passes")

        # Reorg through slim records with undo data (the last 20 blocks hold spends).
        bad = a.getblockhash(tip - 15)
        a.invalidateblock(bad)
        assert_equal(a.getblockcount(), tip - 16)
        a.reconsiderblock(bad)
        assert_equal(a.getbestblockhash(), best)
        # And a longer chain from the legacy node replaces the tip.
        b.invalidateblock(b.getblockhash(tip - 5))
        b.generate(10)
        sync_blocks(self.nodes, timeout=SYNC_TIMEOUT)
        assert_equal(a.getbestblockhash(), b.getbestblockhash())
        print("disconnect, reconnect and a 5 block reorg through slim undo data work after pruning")


if __name__ == '__main__':
    SlimPruneTest().main()
