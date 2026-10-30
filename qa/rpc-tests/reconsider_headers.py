#!/usr/bin/env python3
# Copyright (c) 2013-2026 The Dogecoin Core developers
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.

#
# After invalidateblock followed by reconsiderblock, a peer announcing new headers on top of the
# reconsidered chain must not crash the node. The node used to keep the block in its set of
# failed blocks after clearing its failure flags, and AcceptBlockHeader asserted on that entry
# when it saw a header whose parent descended from it and was not fully validated yet.
#

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *


class ReconsiderHeadersTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.num_nodes = 2
        self.setup_clean_chain = True

    def setup_network(self, split=False):
        self.nodes = start_nodes(self.num_nodes, self.options.tmpdir)
        connect_nodes_bi(self.nodes, 0, 1)
        self.is_network_split = False

    def run_test(self):
        a, b = self.nodes
        b.generate(40)
        sync_blocks(self.nodes)
        tip = b.getblockcount()

        bad = a.getblockhash(tip - 15)
        a.invalidateblock(bad)
        assert_equal(a.getblockcount(), tip - 16)
        a.reconsiderblock(bad)
        assert_equal(a.getblockcount(), tip)

        # A longer chain from the peer that forks 5 blocks back: its headers build on blocks
        # that descend from the reconsidered one.
        b.invalidateblock(b.getblockhash(tip - 5))
        b.generate(10)
        sync_blocks(self.nodes)
        assert_equal(a.getbestblockhash(), b.getbestblockhash())
        assert_equal(a.getblockcount(), tip - 6 + 10)
        print("node 0 follows a reorg on top of a reconsidered block")


if __name__ == '__main__':
    ReconsiderHeadersTest().main()
