#!/usr/bin/env python3
# A language server for close tests. It starts a child that sleeps, writes the
# child's pid to the file named by FAKE_CHILD_PID, answers initialize, and
# leaves on `exit` only when FAKE_EXIT_ON_EXIT is set (the child is left behind
# either way).
import json, os, subprocess, sys, time

child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(600)"])
with open(os.environ["FAKE_CHILD_PID"], "w") as f:
    f.write(str(child.pid))

inp, out = sys.stdin.buffer, sys.stdout.buffer


def read():
    n = None
    while True:
        line = inp.readline()
        if not line:
            return None
        line = line.strip()
        if not line:
            break
        if line.lower().startswith(b"content-length:"):
            n = int(line.split(b":")[1])
    return json.loads(inp.read(n))


while True:
    m = read()
    if m is None:
        time.sleep(600)  # stdin closed: stay up anyway
    if m.get("method") == "initialize":
        b = json.dumps({"jsonrpc": "2.0", "id": m["id"], "result": {"capabilities": {"referencesProvider": True}}}).encode()
        out.write(b"Content-Length: %d\r\n\r\n" % len(b) + b)
        out.flush()
    elif m.get("method") == "exit" and os.environ.get("FAKE_EXIT_ON_EXIT"):
        sys.exit(0)
