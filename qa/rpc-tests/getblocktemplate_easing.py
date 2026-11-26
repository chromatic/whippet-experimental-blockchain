#!/usr/bin/env python3
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.
"""getblocktemplate says how far the time may be rolled under easing.

Under the easing profile the required bits depend on the block's timestamp, but
only once the candidate is more than the free wait (54s) past its parent. The
template reports that point as maxtime and lists "time" as mutable only while
there is room to roll.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *


class GetBlockTemplateEasingTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 2

    def setup_network(self):
        args = ["-timingprofile=scaled-drift-easing", "-timingfloor=4", "-timingeasingtau=30"]
        self.nodes = [start_node(i, self.options.tmpdir, args) for i in range(2)]
        connect_nodes_bi(self.nodes, 0, 1)  # getblocktemplate refuses to run with no peers
        self.is_network_split = False

    def run_test(self):
        node = self.nodes[0]
        t = node.getblockheader(node.getblockhash(0))["time"] + 100
        for _ in range(40):  # blocks 4s apart pull the target below powLimit
            t += 4
            node.setmocktime(t)
            node.generate(1)
        tip_time = t

        def template(at):
            node.setmocktime(at)
            return node.getblocktemplate()

        early = template(tip_time + 5)
        assert_equal(early["maxtime"], tip_time + 54)
        assert "time" in early["mutable"]

        # The bits are the same for every stamp up to maxtime.
        edge = template(tip_time + 54)
        assert_equal(edge["bits"], early["bits"])
        assert_equal(edge["maxtime"], tip_time + 54)

        # One second past it the target has eased, and there is no room left to roll.
        late = template(tip_time + 60)
        assert late["bits"] != early["bits"]
        assert int(late["target"], 16) > int(early["target"], 16)
        assert_equal(late["maxtime"], late["curtime"])
        assert "time" not in late["mutable"]


if __name__ == '__main__':
    GetBlockTemplateEasingTest().main()
