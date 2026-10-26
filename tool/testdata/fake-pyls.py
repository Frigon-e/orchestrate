#!/usr/bin/env python3
# A stand-in for a Python language server for tests: it advertises references
# and implementation but not workspace/symbol, like Pyrefly. References come from
# the JSON file FAKE_LSP_REFS; requests are logged to FAKE_LSP_LOG.
import json, os, sys

log = open(os.environ["FAKE_LSP_LOG"], "a")
refs = json.load(open(os.environ["FAKE_LSP_REFS"]))
out = sys.stdout.buffer
inp = sys.stdin.buffer


def send(obj):
    b = json.dumps(obj).encode()
    out.write(b"Content-Length: %d\r\n\r\n" % len(b) + b)
    out.flush()


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


def note(**kw):
    log.write(json.dumps(kw) + "\n")
    log.flush()


while True:
    m = read()
    if m is None:
        break
    method, p = m.get("method"), m.get("params", {})
    if method is None:
        continue
    if method == "initialize":
        note(method=method, args=sys.argv[1:], root=p["rootUri"])
        send({"jsonrpc": "2.0", "id": m["id"], "result": {"capabilities": {"referencesProvider": True, "implementationProvider": True}}})
    elif method == "textDocument/didOpen":
        note(method=method, uri=p["textDocument"]["uri"], lang=p["textDocument"]["languageId"])
    elif method in ("workspace/symbol", "textDocument/references", "textDocument/implementation"):
        pos = p.get("position", {})
        note(method=method, line=pos.get("line"), character=pos.get("character"))
        send({"jsonrpc": "2.0", "id": m["id"], "result": refs.get(method, [])})
    elif method == "shutdown":
        send({"jsonrpc": "2.0", "id": m["id"], "result": None})
    elif method == "exit":
        break
