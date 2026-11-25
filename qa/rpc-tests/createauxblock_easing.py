#!/usr/bin/env python3
# Distributed under the MIT software license, see the accompanying
# file COPYING or http://www.opensource.org/licenses/mit-license.php.
"""createauxblock serves an eased target while the chain waits.

Under the easing profile the required bits depend on the block's timestamp. A
template built right after a block has the harder bits, and one built long
after it must have easier bits. Before the fix createauxblock kept returning the
first template until the tip changed.
"""

from test_framework.test_framework import BitcoinTestFramework
from test_framework.util import *


class CreateAuxBlockEasingTest(BitcoinTestFramework):

    def __init__(self):
        super().__init__()
        self.setup_clean_chain = True
        self.num_nodes = 1

    def setup_network(self):
        args = ["-timingprofile=scaled-drift-easing", "-timingfloor=4", "-timingeasingtau=30"]
        self.nodes = [start_node(0, self.options.tmpdir, args)]
        self.is_network_split = False

    def run_test(self):
        node = self.nodes[0]
        t = node.getblockheader(node.getblockhash(0))["time"] + 100
        # Blocks 4s apart pull the target below powLimit, so easing has room to show.
        for _ in range(40):
            t += 4
            node.setmocktime(t)
            node.generate(1)
        addr = node.getnewaddress()

        first = node.createauxblock(addr)
        first_target = int(first["bits"], 16)
        again = node.createauxblock(addr)
        assert_equal(again["hash"], first["hash"])  # no wait yet: the same template

        def big(h):
            return int.from_bytes(bytes.fromhex(h), "little")  # target is little-endian hex

        # Past the free wait (54s) the first call rebuilds the template, because the one held is older than the
        # refresh interval. It must carry an easier target than the template built right after the block.
        node.setmocktime(t + 66)
        eased = node.createauxblock(addr)
        assert eased["hash"] != first["hash"], "template was not rebuilt while the chain waited"
        assert_equal(eased["previousblockhash"], first["previousblockhash"])
        assert big(eased["target"]) > big(first["target"]), (eased["target"], first["target"])

        # Inside the refresh interval the same template comes back.
        node.setmocktime(t + 67)
        assert_equal(node.createauxblock(addr)["hash"], eased["hash"])

        # After the interval it is rebuilt again, with a larger target still.
        node.setmocktime(t + 66 + 11)
        eased2 = node.createauxblock(addr)
        assert eased2["hash"] != eased["hash"]
        assert big(eased2["target"]) > big(eased["target"]), (eased2["target"], eased["target"])


if __name__ == '__main__':
    CreateAuxBlockEasingTest().main()
