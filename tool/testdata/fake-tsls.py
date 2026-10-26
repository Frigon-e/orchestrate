#!/usr/bin/env python3
# A stand-in for the TypeScript 7 native language server (`tsc --lsp --stdio`)
# for tests: speaks LSP framing on stdio and answers from JSON fixtures named by
# environment variables.
import json, os, sys

log = open(os.environ["FAKE_LSP_LOG"], "a")
symbols = json.load(open(os.environ["FAKE_LSP_SYMBOLS"]))
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
    if method is None:  # a reply to a request of ours
        note(reply=m.get("id"), result=m.get("result"))
        continue
    if method == "initialize":
        note(method=method, args=sys.argv[1:], root=p["rootUri"], folders=[f["uri"] for f in p["workspaceFolders"]],
             config=p["capabilities"]["workspace"]["configuration"], watch=p["capabilities"]["workspace"]["didChangeWatchedFiles"]["dynamicRegistration"])
        send({"jsonrpc": "2.0", "id": m["id"], "result": {"capabilities": {}}})
    elif method == "initialized":
        note(method=method)
        # Like the real server, ask for configuration and register capabilities; it waits for the answers.
        send({"jsonrpc": "2.0", "id": 900, "method": "client/registerCapability", "params": {}})
        send({"jsonrpc": "2.0", "id": 901, "method": "workspace/configuration", "params": {"items": [{"section": "js/ts"}, {"section": "typescript"}]}})
    elif method == "textDocument/didOpen":
        note(method=method, uri=p["textDocument"]["uri"], lang=p["textDocument"]["languageId"])
    elif method == "workspace/symbol":
        note(method=method, query=p["query"])
        if os.environ.get("FAKE_LSP_HANG"):
            continue
        cold = int(os.environ.get("FAKE_LSP_COLD", "0"))
        seen = globals().setdefault("seen_symbol", 0) + 1
        globals()["seen_symbol"] = seen
        send({"jsonrpc": "2.0", "id": m["id"], "result": [] if seen <= cold else symbols.get(p["query"], [])})
    elif method == "textDocument/references":
        pos = p["position"]
        note(method=method, uri=p["textDocument"]["uri"], line=pos["line"], character=pos["character"], decl=p["context"]["includeDeclaration"])
        send({"jsonrpc": "2.0", "id": m["id"], "result": refs.get("default", [])})
    elif method == "shutdown":
        note(method=method)
        send({"jsonrpc": "2.0", "id": m["id"], "result": None})
    elif method == "exit":
        note(method=method)
        break
