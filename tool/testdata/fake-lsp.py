#!/usr/bin/env python3
# A stand-in for gopls in `serve` mode for tests: speaks LSP framing on stdio,
# logs what it is told to the file named by FAKE_LSP_LOG and answers a fixed
# call hierarchy, outline, hover and implementation.
import json, os, sys

log = open(os.environ["FAKE_LSP_LOG"], "a")
repo = os.environ["FAKE_LSP_REPO"]
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


def loc(path, line):
    return {"uri": "file://" + repo + "/" + path, "range": {"start": {"line": line, "character": 0}, "end": {"line": line, "character": 5}}}


note(method="start", pid=os.getpid(), args=sys.argv[1:])
while True:
    m = read()
    if m is None:
        break
    method, p = m.get("method"), m.get("params") or {}
    if method is None:
        continue
    note(method=method, has_params="params" in m)
    rid = m.get("id")
    if method == "initialize":
        note(method="caps", watch=p["capabilities"]["workspace"]["didChangeWatchedFiles"]["dynamicRegistration"])
        send({"jsonrpc": "2.0", "id": rid, "result": {"capabilities": {
            "referencesProvider": True, "implementationProvider": True, "callHierarchyProvider": True,
            "documentSymbolProvider": True, "hoverProvider": True, "definitionProvider": True}}})
    elif method == "initialized":
        send({"jsonrpc": "2.0", "id": 900, "method": "client/registerCapability", "params": {}})
    elif method == "textDocument/didOpen":
        note(method="open", uri=p["textDocument"]["uri"], text=p["textDocument"]["text"])
    elif method == "textDocument/didClose":
        note(method="close", uri=p["textDocument"]["uri"])
    elif method == "workspace/didChangeWatchedFiles":
        note(method="watched", changes=p["changes"])
    elif method == "textDocument/prepareCallHierarchy":
        send({"jsonrpc": "2.0", "id": rid, "result": [{"name": "StartCheckout", "uri": p["textDocument"]["uri"]}]})
    elif method == "callHierarchy/incomingCalls":
        send({"jsonrpc": "2.0", "id": rid, "result": [{"from": {"name": "Handle", "uri": "file://" + repo + "/svc/svc.go"},
              "fromRanges": [{"start": {"line": 6, "character": 8}}]}]})
    elif method == "textDocument/implementation":
        send({"jsonrpc": "2.0", "id": rid, "result": [loc("pay/pay.go", 6), loc("svc/svc.go", 5)]})
    elif method == "textDocument/documentSymbol":
        send({"jsonrpc": "2.0", "id": rid, "result": [{"name": "Stripe", "kind": 23, "range": loc("pay/pay.go", 5)["range"], "selectionRange": loc("pay/pay.go", 5)["range"]}]})
    elif method == "textDocument/hover":
        send({"jsonrpc": "2.0", "id": rid, "result": {"contents": "type Stripe struct"}})
    elif method == "textDocument/references":
        send({"jsonrpc": "2.0", "id": rid, "result": [loc("svc/svc.go", 6)]})
    elif method == "shutdown":
        send({"jsonrpc": "2.0", "id": rid, "result": None})
    elif method == "exit":
        break
    elif rid is not None:
        send({"jsonrpc": "2.0", "id": rid, "result": None})
