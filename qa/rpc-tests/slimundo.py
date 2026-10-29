#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

#
# Test -slimundo: no undo record (rev*.dat) for a block that spent nothing, and every path that
# needs an undo (disconnecting blocks, getblockstats, verifychain) still works, with or without
# the option on the next run.
#

import os
import struct

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *

REGTEST_MAGIC = bytes.fromhex('fabfb5da')


def parse_undo_records(path):
    """Records in a rev file as (checksum_bytes, offset) pairs: magic, size word, undo, checksum.
    Bit 31 of the size word marks a short (8 byte) checksum, otherwise it is 32 bytes."""
    data = open(path, 'rb').read()
    pos, recs = 0, []
    while data[pos:pos + 4] == REGTEST_MAGIC:
        word = struct.unpack_from('<I', data, pos + 4)[0]
        cks = 8 if word & 0x80000000 else 32
        recs.append((cks, pos))
        pos += 8 + (word & 0x7fffffff) + cks
    # Every record length was knowable: all that follows is the zero padding of a preallocated file.
    assert_equal(data[pos:].count(0), len(data) - pos)
    return recs


def count_undo_records(path):
    return len(parse_undo_records(path))


class SlimUndoTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        self.nodes = [start_node(0, self.options.tmpdir, ["-debug", "-slimundo=1"])]
        self.is_network_split = False

    def rev_path(self):
        return os.path.join(self.options.tmpdir, "node0", "regtest", "blocks", "rev00000.dat")

    def restart(self, extra):
        stop_node(self.nodes[0], 0)
        self.nodes[0] = start_node(0, self.options.tmpdir, ["-debug"] + extra)

    def run_test(self):
        node = self.nodes[0]
        node.generate(110)
        assert_equal(count_undo_records(self.rev_path()), 0)
        print("110 coinbase-only blocks wrote no undo record")

        spending_heights = []
        for i in range(3):
            node.sendtoaddress(node.getnewaddress(), 1)
            node.generate(1)
            spending_heights.append(node.getblockcount())
            node.generate(2)   # coinbase-only blocks in between
        tip = node.getblockcount()
        assert_equal(count_undo_records(self.rev_path()), 3)
        assert_equal([c for c, _ in parse_undo_records(self.rev_path())], [8, 8, 8])
        print("3 spending blocks wrote 3 undo records with an 8 byte checksum, the %d coinbase-only blocks none" % (tip - 3))

        # getblockstats reads the undo of every block: coinbase-only and spending.
        for h in range(1, tip + 1):
            stats = node.getblockstats(node.getblockhash(h))
            assert_equal(stats["height"], h)

        # Disconnect across both kinds of block, then connect them again.
        bad = node.getblockhash(111)
        node.invalidateblock(bad)
        assert_equal(node.getblockcount(), 110)
        node.reconsiderblock(bad)
        assert_equal(node.getblockcount(), tip)
        best = node.getbestblockhash()
        assert_equal(count_undo_records(self.rev_path()), 3)
        assert node.verifychain(4, 0)
        print("invalidate/reconsider across spending and coinbase-only blocks works")

        # The next run without the option still reads everything, and writes a record for new blocks again.
        self.restart([])
        node = self.nodes[0]
        assert_equal(node.getbestblockhash(), best)
        assert_equal(count_undo_records(self.rev_path()), 3)
        # verifychain level 4 reconnects every block, so without the option each one gets a record.
        assert node.verifychain(4, 0)
        assert_equal(count_undo_records(self.rev_path()), tip)
        # The first 3 records stay short, the ones written without the option are legacy (32 byte).
        sizes = [c for c, _ in parse_undo_records(self.rev_path())]
        assert_equal(sizes.count(8), 3)
        assert_equal(sizes.count(32), tip - 3)
        node.invalidateblock(bad)
        assert_equal(node.getblockcount(), 110)
        node.reconsiderblock(bad)
        assert_equal(node.getblockcount(), tip)
        node.generate(1)
        assert_equal(count_undo_records(self.rev_path()), tip + 1)
        assert_equal(parse_undo_records(self.rev_path())[-1][0], 32)
        # Mixed file: disconnect and reconnect across short and legacy records, then once more with the option.
        self.restart(["-slimundo=1"])
        node = self.nodes[0]
        node.invalidateblock(bad)
        node.reconsiderblock(bad)
        assert_equal(node.getblockcount(), tip + 1)
        assert node.verifychain(4, 0)
        node.sendtoaddress(node.getnewaddress(), 1)
        node.generate(1)
        assert_equal(parse_undo_records(self.rev_path())[-1][0], 8)
        assert_equal(count_undo_records(self.rev_path()), tip + 2)
        node.invalidateblock(node.getbestblockhash())
        assert_equal(node.getblockcount(), tip + 1)
        print("restart without -slimundo reads the same data and writes records for the blocks it connects")


if __name__ == '__main__':
    SlimUndoTest().main()
