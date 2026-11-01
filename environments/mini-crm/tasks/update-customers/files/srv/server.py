"""Mini CRM: a JSON API over SQLite. Every write is recorded in `audit`."""
import json
import re
import socketserver
import sqlite3
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DB = sys.argv[1]
TIERS = {"free", "pro", "enterprise"}
EMAIL = re.compile(r"^[^@\s]+@[^@\s]+\.[^@\s]+$")


def conn():
    c = sqlite3.connect(DB, timeout=5)
    c.row_factory = sqlite3.Row
    return c


def row(r):
    return {"id": r["id"], "name": r["name"], "email": r["email"], "tier": r["tier"]}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n > 65536:
            raise ValueError("body too large")
        return json.loads(self.rfile.read(n) or b"{}")

    def cid(self):
        m = re.fullmatch(r"/customers/(\d+)", self.path)
        return int(m.group(1)) if m else None

    def do_GET(self):
        if self.path == "/health":
            return self.reply(200, {"ok": True})
        with conn() as c:
            if self.path == "/customers":
                return self.reply(200, [row(r) for r in c.execute("SELECT * FROM customers ORDER BY id")])
            i = self.cid()
            r = c.execute("SELECT * FROM customers WHERE id=?", (i,)).fetchone() if i else None
            return self.reply(200, row(r)) if r else self.reply(404, {"error": "not found"})

    def validate(self, fields, partial):
        out = {}
        for k in ("name", "email", "tier"):
            if k not in fields:
                if not partial:
                    raise ValueError("missing " + k)
                continue
            v = fields[k]
            if not isinstance(v, str) or not v.strip():
                raise ValueError("bad " + k)
            if k == "email" and not EMAIL.match(v):
                raise ValueError("bad email")
            if k == "tier" and v not in TIERS:
                raise ValueError("tier must be one of %s" % sorted(TIERS))
            out[k] = v.strip()
        if partial and not out:
            raise ValueError("nothing to update")
        return out

    def write(self, fn):
        try:
            with conn() as c:
                return fn(c)
        except (ValueError, sqlite3.IntegrityError) as e:
            return self.reply(400, {"error": str(e)})

    def do_POST(self):
        if self.path != "/customers":
            return self.reply(404, {"error": "not found"})

        def fn(c):
            f = self.validate(self.body(), partial=False)
            cur = c.execute("INSERT INTO customers (name, email, tier) VALUES (?, ?, ?)", (f["name"], f["email"], f["tier"]))
            c.execute("INSERT INTO audit (op, customer_id, detail) VALUES ('create', ?, ?)", (cur.lastrowid, json.dumps(f)))
            return self.reply(201, row(c.execute("SELECT * FROM customers WHERE id=?", (cur.lastrowid,)).fetchone()))
        self.write(fn)

    def do_PATCH(self):
        i = self.cid()

        def fn(c):
            if not i or not c.execute("SELECT 1 FROM customers WHERE id=?", (i,)).fetchone():
                return self.reply(404, {"error": "not found"})
            f = self.validate(self.body(), partial=True)
            sets = ", ".join("%s=?" % k for k in f)
            c.execute("UPDATE customers SET %s WHERE id=?" % sets, (*f.values(), i))
            c.execute("INSERT INTO audit (op, customer_id, detail) VALUES ('update', ?, ?)", (i, json.dumps(f)))
            return self.reply(200, row(c.execute("SELECT * FROM customers WHERE id=?", (i,)).fetchone()))
        self.write(fn)

    def do_DELETE(self):
        i = self.cid()

        def fn(c):
            if not i or c.execute("DELETE FROM customers WHERE id=?", (i,)).rowcount == 0:
                return self.reply(404, {"error": "not found"})
            c.execute("INSERT INTO audit (op, customer_id, detail) VALUES ('delete', ?, '{}')", (i,))
            return self.reply(200, {"deleted": i})
        self.write(fn)


class Server(ThreadingHTTPServer):
    def server_bind(self):
        # HTTPServer.server_bind does a reverse DNS lookup (socket.getfqdn),
        # which stalls for seconds in a sandbox without network.
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "127.0.0.1", self.server_address[1]


Server(("127.0.0.1", 8080), Handler).serve_forever()
