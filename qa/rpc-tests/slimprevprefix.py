#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

#
# Test -slimprevprefix: slim blk records keep only 8 bytes of the previous block hash.
# - normal reads (getblock, getblockheader, verifychain) take the parent from the block index
# - the txindex finds the block hash of a transaction in a prefix-only record
# - -reindex and -loadblock resolve the prefixes against blocks they have accepted, also when the
#   records are in the wrong order (every child before its parent, genesis last)
#

import os
import struct
import time

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *

REGTEST_MAGIC = bytes.fromhex('fabfb5da')
SLIM_FLAG = 0x80000000
FLAG_PREV_PREFIX = 1 << 3


def read_records(path):
    """Raw records (magic, size word and payload) of blk00000.dat; the rest is zero padding."""
    data = open(path, 'rb').read()
    pos, recs = 0, []
    while data[pos:pos + 4] == REGTEST_MAGIC:
        word = struct.unpack_from('<I', data, pos + 4)[0]
        size = word & 0x7fffffff
        recs.append(data[pos:pos + 8 + size])
        pos += 8 + size
    return recs


def is_prefix_record(rec):
    word = struct.unpack_from('<I', rec, 4)[0]
    return bool(word & SLIM_FLAG) and bool(rec[8] & FLAG_PREV_PREFIX)


class SlimPrevPrefixTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.num_nodes = 3
        self.setup_clean_chain = True

    def setup_network(self, split=False):
        self.nodes = [start_node(0, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks", "-slimprevprefix"])]
        self.is_network_split = False

    def datadir_blocks(self, n):
        return os.path.join(self.options.tmpdir, "node%d" % n, "regtest", "blocks")

    def check_chain(self, node, ref, tip):
        assert_equal(node.getblockcount(), tip)
        assert_equal(node.getbestblockhash(), ref.getbestblockhash())
        for h in range(tip + 1):
            hash_ = ref.getblockhash(h)
            assert_equal(node.getblockhash(h), hash_)
            assert_equal(node.getblock(hash_, False), ref.getblock(hash_, False))
            assert_equal(node.getblockheader(hash_, False), ref.getblockheader(hash_, False))

    def count_out_of_order(self, n):
        log = open(os.path.join(self.options.tmpdir, "node%d" % n, "regtest", "debug.log"), errors="replace").read()
        return log.count("Out of order block")

    def run_test(self):
        a = self.nodes[0]
        a.generate(70)
        addr = a.getnewaddress()
        for i in range(5):
            a.sendtoaddress(addr, 1)
            a.sendtoaddress(addr, 2)
            a.generate(1)      # multi-tx
            a.generate(2)      # single-tx
        tip = a.getblockcount()

        # Records: every block but genesis has the prefix flag.
        recs = read_records(os.path.join(self.datadir_blocks(0), "blk00000.dat"))
        assert_equal(len(recs), tip + 1)
        n_prefix = sum(1 for r in recs if is_prefix_record(r))
        assert_equal(n_prefix, tip)
        print("%d of %d records keep a prefix of the parent hash" % (n_prefix, len(recs)))

        # Normal reads and the txindex, which cannot compute the header hash of such a record.
        self.check_chain(a, a, tip)
        n_tx = 0
        for h in range(1, tip + 1):
            blk = a.getblock(a.getblockhash(h))
            for txid in blk["tx"]:
                assert_equal(a.getrawtransaction(txid, 1)["blockhash"], blk["hash"])
                n_tx += 1
        assert a.verifychain(4, 0)
        print("getblock, getblockheader, verifychain and the txindex (%d transactions) read prefix-only records" % n_tx)

        # Reversed: genesis last, every block before its parent.
        stop_node(a, 0)
        rev = b"".join(reversed(recs))

        # -reindex: the records are on disk (dbp), parents are looked up by prefix as they arrive.
        os.makedirs(self.datadir_blocks(1))
        open(os.path.join(self.datadir_blocks(1), "blk00000.dat"), "wb").write(rev)
        self.nodes = [start_node(0, self.options.tmpdir, ["-debug", "-txindex"])]   # reference, normal order
        ref = self.nodes[0]
        n1 = start_node(1, self.options.tmpdir, ["-debug", "-txindex", "-slimblocks", "-slimprevprefix", "-reindex"])
        self.nodes.append(n1)
        self.check_chain(n1, ref, tip)
        assert self.count_out_of_order(1) >= tip - 1
        assert n1.verifychain(4, 0)
        print("-reindex of a reversed prefix-only file: %d blocks, %d out of order" % (tip + 1, self.count_out_of_order(1)))

        # -loadblock: no disk position, so the blocks wait in memory.
        rev_path = os.path.join(self.options.tmpdir, "reversed.dat")
        open(rev_path, "wb").write(rev)
        n2 = start_node(2, self.options.tmpdir, ["-debug", "-loadblock=" + rev_path])
        self.nodes.append(n2)
        wait_until = 0
        while n2.getblockcount() < tip and wait_until < 600:
            time.sleep(0.1)
            wait_until += 1
        self.check_chain(n2, ref, tip)
        assert self.count_out_of_order(2) >= tip - 1
        print("-loadblock of a reversed prefix-only file: %d blocks, %d out of order" % (tip + 1, self.count_out_of_order(2)))


if __name__ == '__main__':
    SlimPrevPrefixTest().main()
