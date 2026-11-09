#!/usr/bin/env python3
"""Run one local QA command in a bounded systemd user service; never fall back."""

import argparse
import os
import shutil
import subprocess
import sys
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=int, default=180)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command or not 1 <= args.seconds <= 1800:
        parser.error("provide a command and a deadline of 1..1800 seconds")

    rtk = shutil.which("rtk")
    executable = shutil.which(command[0])
    if rtk is None or executable is None:
        parser.error("rtk and the requested command must be installed")
    command[0] = executable
    unit = "emlkit-bounded-" + uuid.uuid4().hex[:10]
    runner = [
        rtk, "proxy", "systemd-run", "--user", "--wait", "--pipe",
        "--collect", "--quiet", "--unit=" + unit,
        "-p", "MemoryMax=1536M",
        "-p", "MemorySwapMax=0",
        "-p", "CPUQuota=200%",
        "-p", "TasksMax=128",
        "-p", "RuntimeMaxSec=" + str(args.seconds),
        "-p", "TimeoutStopSec=5",
        "-p", "OOMPolicy=kill",
        "--working-directory=" + os.getcwd(),
        "--setenv=PATH=" + os.environ.get("PATH", os.defpath),
        "--setenv=GOMAXPROCS=2",
        "--setenv=GOMEMLIMIT=512MiB",
        "--setenv=GOFLAGS=-p=2",
    ]
    print(
        f"Bounded unit {unit}: RAM 1536 MiB, swap 0, CPU quota 2, "
        f"tasks 128, deadline {args.seconds}s",
        file=sys.stderr, flush=True,
    )
    # systemd applies the limits before starting the command, retains its exit
    # status, and terminates the entire unit on OOM or deadline. No fallback.
    return subprocess.run(runner + command).returncode


if __name__ == "__main__":
    sys.exit(main())
