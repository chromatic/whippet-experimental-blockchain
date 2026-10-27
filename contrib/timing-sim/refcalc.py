"""Client for the timing_ref reference calculator (src/test/timing_ref.cpp).

The binary only builds inside the whippet-ubuntu24 image, so by default it is
run with `docker run -i`. Set WHIPPET_TIMING_REF to a path to run a natively
built binary instead.
"""
import os
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
IMAGE = os.environ.get("WHIPPET_TIMING_IMAGE", "whippet-ubuntu24")


class RefCalcError(RuntimeError):
    pass


class RefCalc:
    """One persistent timing_ref process. Use as a context manager."""

    def __init__(self, profile: str, floor: int = 4, min_difficulty: bool = False,
                 easing_tau: int = 30):
        args = [f"-timingprofile={profile}", f"-timingfloor={floor}", f"-timingmindiff={int(min_difficulty)}",
                f"-timingeasingtau={easing_tau}"]
        native = os.environ.get("WHIPPET_TIMING_REF")
        if native:
            cmd = [native] + args
        else:
            cmd = ["docker", "run", "-i", "--rm", "-u", f"{os.getuid()}:{os.getgid()}",
                   "-v", f"{REPO}:/src", "--entrypoint", "/src/src/test/timing_ref", IMAGE] + args
        self._proc = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      text=True, bufsize=1)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def close(self) -> None:
        if self._proc.poll() is None:
            self._proc.stdin.close()
            self._proc.wait(timeout=10)

    def command(self, line: str) -> str:
        self._proc.stdin.write(line + "\n")
        self._proc.stdin.flush()
        reply = self._proc.stdout.readline().rstrip("\n")
        if not reply:
            raise RefCalcError("timing_ref exited (check profile and floor arguments)")
        if reply.startswith("ERR"):
            raise RefCalcError(reply)
        return reply

    def reset(self, height: int = 145000, bits: int = 0x207FFFFF) -> None:
        self.command(f"RESET {height} {bits:x}")

    def append(self, time: int, bits: int) -> int:
        return int(self.command(f"APPEND {time} {bits:x}").split()[1])

    def tip(self, index: int) -> None:
        self.command(f"TIP {index}")

    def next_bits(self, time: int) -> int:
        return int(self.command(f"NEXTBITS {time}").split()[1], 16)

    def check(self, time: int, bits: int, adjusted: int):
        """None if the header is acceptable, else the reject reason."""
        reply = self.command(f"CHECK {time} {bits:x} {adjusted}")
        return None if reply == "OK" else reply.split(" ", 1)[1]
