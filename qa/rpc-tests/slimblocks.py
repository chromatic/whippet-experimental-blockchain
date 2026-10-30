#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

#
# Test -slimblocks: blocks written to blk*.dat without the header fields that can be recomputed
# (merkle root of a single-transaction block, derivable AuxPoW fields). Every block must read back
# byte for byte, through getblock, the txindex, a restart with and without the option, and -reindex.
# Node 0 writes slim records, node 1 legacy ones; they follow the same chain.
#

import os
import struct
import time

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *
from test_framework import scrypt_auxpow

REGTEST_MAGIC = bytes.fromhex('fabfb5da')
SLIM_FLAG = 0x80000000


def blk_records(path):
    """(slim, payload_size) for each record in blk00000.dat; the rest is zero padding."""
    data = open(path, 'rb').read()
    pos, recs = 0, []
    while data[pos:pos + 4] == REGTEST_MAGIC:
        word = struct.unpack_from('<I', data, pos + 4)[0]
        size = word & 0x7fffffff
        recs.append((bool(word & SLIM_FLAG), size))
        pos += 8 + size
    assert_equal(data[pos:].count(0), len(data) - pos)
    return recs


class SlimBlocksTest(BitcoinTestFramework):
    AUXPOW_START = 20   # regtest height from which merge-mined blocks are accepted

    def __init__(self):
        super().__init__()
        self.num_nodes = 2
        self.setup_clean_chain = True

    def setup_network(self, split=False):
        self.nodes = [
            start_node(0, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks=1"]),
            start_node(1, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks=0"]),
        ]
        connect_nodes_bi(self.nodes, 0, 1)
        self.is_network_split = False

    def blk_path(self, n):
        return os.path.join(self.options.tmpdir, "node%d" % n, "regtest", "blocks", "blk00000.dat")

    def restart(self, n, extra):
        stop_node(self.nodes[n], n)
        self.nodes[n] = start_node(n, self.options.tmpdir, ["-debug", "-txindex"] + extra)
        connect_nodes_bi(self.nodes, 0, 1)

    def wait_for_height(self, node, height):
        """A -reindex runs in the import thread after the RPC server is up."""
        for _ in range(3000):
            if node.getblockcount() >= height:
                return
            time.sleep(0.1)

    def check_same(self, tip):
        """Both nodes serve identical blocks and transactions for the whole chain."""
        a, b = self.nodes
        assert_equal(a.getbestblockhash(), b.getbestblockhash())
        txids = []
        for h in range(0, tip + 1):  # the genesis coinbase is not in the txindex, skipped below
            hash_ = a.getblockhash(h)
            assert_equal(a.getblock(hash_, False), b.getblock(hash_, False))
            assert_equal(a.getblockheader(hash_, False), b.getblockheader(hash_, False))
            if h > 0:
                txids.extend(a.getblock(hash_)["tx"])
        for txid in txids:
            ra, rb = a.getrawtransaction(txid, 1), b.getrawtransaction(txid, 1)
            assert_equal(ra, rb)
        return len(txids)

    def run_test(self):
        a, b = self.nodes
        a.generate(self.AUXPOW_START + 60)   # single-tx blocks; coins mature after 60 blocks
        self.sync_all()
        addr = b.getnewaddress()

        # Mix single-tx and multi-tx blocks, merge-mined and not.
        for i in range(4):
            assert scrypt_auxpow.mineScryptAux(a, "00", True) is True      # AuxPoW, single tx
            a.sendtoaddress(addr, 1)
            a.sendtoaddress(addr, 2)
            assert scrypt_auxpow.mineScryptAux(a, "00", True) is True      # AuxPoW, 3 txs
            a.sendtoaddress(addr, 1)
            a.generate(1)                                                  # legacy-PoW block, 2 txs
            a.generate(1)                                                  # legacy-PoW block, 1 tx
        self.sync_all()
        tip = a.getblockcount()
        assert_equal(b.getblockcount(), tip)

        slim = blk_records(self.blk_path(0))
        legacy = blk_records(self.blk_path(1))
        assert_equal(len(slim), len(legacy))
        n_slim = sum(1 for s, _ in slim if s)
        assert n_slim > 0
        assert not any(s for s, _ in legacy)
        size_slim = sum(sz for _, sz in slim)
        size_legacy = sum(sz for _, sz in legacy)
        assert size_slim < size_legacy
        print("%d of %d records slim; payload bytes %d vs %d legacy (%.1f%% saved)" % (
            n_slim, len(slim), size_slim, size_legacy, 100.0 * (size_legacy - size_slim) / size_legacy))

        ntx = self.check_same(tip)
        print("%d blocks and %d transactions identical on the slim and the legacy node (getblock, getblockheader, txindex)" % (tip + 1, ntx))

        # Restart node 0 without the option: old records still read, new blocks are legacy.
        self.restart(0, ["-slimblocks=0"])
        a = self.nodes[0]
        self.check_same(tip)
        a.generate(2)
        a.sendtoaddress(addr, 1)
        assert scrypt_auxpow.mineScryptAux(a, "00", True) is True
        self.sync_all()
        recs = blk_records(self.blk_path(0))
        assert not any(s for s, _ in recs[len(slim):])
        tip = a.getblockcount()
        print("restart without -slimblocks reads the slim records and appends legacy ones")

        # And back on: a file with both kinds of record.
        self.restart(0, ["-slimblocks=1"])
        a = self.nodes[0]
        a.generate(2)
        a.sendtoaddress(addr, 1)
        assert scrypt_auxpow.mineScryptAux(a, "00", True) is True
        self.sync_all()
        tip = a.getblockcount()
        kinds = [s for s, _ in blk_records(self.blk_path(0))]
        assert True in kinds and False in kinds
        assert kinds.index(False) < len(kinds) - 1 and kinds[-1] is True
        self.check_same(tip)
        assert a.verifychain(4, 0)
        print("mixed legacy and slim file reads and verifychain level 4 passes")

        # -reindex rebuilds the block index, chainstate and txindex from the mixed file.
        best = a.getbestblockhash()
        stop_node(a, 0)
        self.nodes[0] = start_node(0, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks=1", "-reindex"])
        connect_nodes_bi(self.nodes, 0, 1)
        a = self.nodes[0]
        self.wait_for_height(a, tip)
        assert_equal(a.getbestblockhash(), best)
        assert_equal(a.getblockcount(), tip)
        self.check_same(tip)
        # the reindexed node can disconnect and connect across slim and legacy blocks
        bad = a.getblockhash(self.AUXPOW_START + 1)
        a.invalidateblock(bad)
        assert_equal(a.getblockcount(), self.AUXPOW_START)
        a.reconsiderblock(bad)
        assert_equal(a.getbestblockhash(), best)
        assert a.verifychain(4, 0)
        print("-reindex from the mixed file restores the same chain, txindex and undo data")

        # A reindex with the option off reads the same file.
        stop_node(a, 0)
        self.nodes[0] = start_node(0, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks=0", "-reindex"])
        connect_nodes_bi(self.nodes, 0, 1)
        self.wait_for_height(self.nodes[0], tip)
        assert_equal(self.nodes[0].getbestblockhash(), best)
        self.check_same(tip)
        print("-reindex without the option reads slim records too")


if __name__ == '__main__':
    SlimBlocksTest().main()
