The [pull-tester](/qa/pull-tester/) folder contains a script to call
multiple tests from the [rpc-tests](/qa/rpc-tests/) folder.

Every pull request to the whippet repository is built and run through
the regression test suite. You can also run all or only individual
tests locally.

Test dependencies
=================
Before running the tests, the following must be installed.

Unix
----
`python3-zmq` is required. On Ubuntu or Debian:
```
sudo apt-get update
sudo apt-get install -y python3-zmq
```

OS X
------
```
pip3 install pyzmq
```

Scrypt proof-of-work
--------------------
The tests hash scrypt proof-of-work through
`qa/rpc-tests/test_framework/scrypt_pow.py`, which uses the `ltc_scrypt`
C extension when it is installed and otherwise falls back to
`hashlib.scrypt` from the standard library. Both produce identical
results — run that file directly to check them against upstream
ltc-scrypt's known-answer vectors:
```
python3 qa/rpc-tests/test_framework/scrypt_pow.py
```

Installing the extension is therefore optional. If you want it anyway:
```
sudo apt-get install -y curl gcc python3-pip python3-setuptools
./qa/pull-tester/install-deps.sh
```
Note that on any PEP 668 distribution — which now includes current
Debian and Ubuntu — that script's `pip install --user` is refused with
`error: externally-managed-environment`. Install it into a virtualenv, or
just rely on the stdlib fallback.

Running tests
=============

You can run any single test by calling

    qa/pull-tester/rpc-tests.py <testname>

Or you can run any combination of tests by calling

    qa/pull-tester/rpc-tests.py <testname1> <testname2> <testname3> ...

Run the regression test suite with

    qa/pull-tester/rpc-tests.py

Run all possible tests with

    qa/pull-tester/rpc-tests.py -extended

By default, tests will be run in parallel. To specify how many jobs to run,
append `-parallel=n` (default n=4).

If you want to create a basic coverage report for the rpc test suite, append `--coverage`.

Possible options, which apply to each individual test run:

```
  -h, --help            show this help message and exit
  --nocleanup           Leave whippetds and test.* datadir on exit or error
  --noshutdown          Don't stop whippetds after the test execution
  --srcdir=SRCDIR       Source directory containing whippetd/whippet-cli
                        (default: ../../src)
  --tmpdir=TMPDIR       Root directory for datadirs
  --tracerpc            Print out all RPC calls as they are made
  --coveragedir=COVERAGEDIR
                        Write tested RPC commands into this directory
```

If you set the environment variable `PYTHON_DEBUG=1` you will get some debug
output (example: `PYTHON_DEBUG=1 qa/pull-tester/rpc-tests.py wallet`).

A 200-block -regtest blockchain and wallets for four nodes
is created the first time a regression test is run and
is stored in the cache/ directory. Each node has 25 mature
blocks (25*50=1250 BTC) in its wallet.

After the first run, the cache/ blockchain and wallets are
copied into a temporary directory and used as the initial
test state.

If you get into a bad state, you should be able
to recover with:

```bash
rm -rf cache
killall whippetd
```

Writing tests
=============
You are encouraged to write tests for new or existing features.
Further information about the test framework and individual rpc
tests is found in [qa/rpc-tests](/qa/rpc-tests).
