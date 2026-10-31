#!/opt/homebrew/bin/python3
"""Serve several Plannotator reviews under one host, one path each.

  https://review.chiq.me/<id>/   ->  the review registered as <id>

Plannotator's UI is a single bundle that calls absolute paths (/api/..., /favicon.png),
so it cannot be mounted under a prefix as-is. The router therefore:
  * strips /<id> before forwarding to the review's own port;
  * injects a shim into the HTML page that prefixes fetch, XHR and EventSource URLs;
  * routes anything that still arrives unprefixed (images, favicon) by the page's Referer,
    and, when exactly one review is registered, sends it there.

A review is registered by a file REGISTRY/<id> holding its port (written by
plannotator-review.sh, removed when the review ends). Nothing is kept in memory.
GET / lists the registered reviews.
"""
import html
import http.client
import json
import os
import re
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

LISTEN_PORT = int(os.environ.get("PLANNOTATOR_ROUTER_PORT", "19432"))
REGISTRY = os.environ.get("PLANNOTATOR_ROUTER_DIR", os.path.expanduser("~/.plannotator-router"))
ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
HOP = {"connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te",
       "trailers", "transfer-encoding", "upgrade"}

SHIM = """<script>(function(){var P=%s;
function fix(u){if(typeof u==='string'&&u.charAt(0)==='/'&&u.charAt(1)!=='/'&&u.indexOf(P+'/')!==0)return P+u;return u}
var f=window.fetch;window.fetch=function(i,o){
 if(typeof i==='string')i=fix(i);
 else if(i&&i.url){try{var u=new URL(i.url,location.href);if(u.origin===location.origin&&u.pathname.indexOf(P+'/')!==0)i=new Request(P+u.pathname+u.search,i)}catch(e){}}
 return f.call(this,i,o)};
var o=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){arguments[1]=fix(u);return o.apply(this,arguments)};
var E=window.EventSource;window.EventSource=function(u,c){return new E(fix(u),c)};window.EventSource.prototype=E.prototype;
['CONNECTING','OPEN','CLOSED'].forEach(function(k,n){window.EventSource[k]=n});
})()</script>"""


def registered():
    try:
        names = os.listdir(REGISTRY)
    except FileNotFoundError:
        return {}
    out = {}
    for n in names:
        if ID_RE.match(n):
            try:
                out[n] = int(open(os.path.join(REGISTRY, n)).read().strip())
            except (OSError, ValueError):
                pass
    return out


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s %s\n" % (self.address_string(), fmt % args))

    def reply(self, code, body, ctype="text/plain; charset=utf-8", extra=()):
        data = body.encode() if isinstance(body, str) else body
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        for k, v in extra:
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(data)

    def route(self):
        """Return (id, port, path-with-query-for-the-review) or None."""
        reviews = registered()
        parts = urlsplit(self.path)
        m = re.match(r"^/([a-z0-9][a-z0-9-]*)(/.*)?$", parts.path)
        if m and m.group(1) in reviews:
            rest = (m.group(2) or "/") + ("?" + parts.query if parts.query else "")
            return m.group(1), reviews[m.group(1)], rest, m.group(2) is None
        ref = urlsplit(self.headers.get("Referer", "")).path
        m = re.match(r"^/([a-z0-9][a-z0-9-]*)(/|$)", ref)
        if m and m.group(1) in reviews:
            return m.group(1), reviews[m.group(1)], self.path, False
        if len(reviews) == 1:
            (rid, port), = reviews.items()
            return rid, port, self.path, False
        return None

    def handle_any(self):
        if self.path == "/" or self.path.startswith("/?"):
            reviews = registered()
            if self.headers.get("Accept", "").startswith("application/json"):
                return self.reply(200, json.dumps(reviews), "application/json")
            items = "".join('<li><a href="/%s/">%s</a></li>' % (html.escape(i), html.escape(i))
                            for i in sorted(reviews)) or "<li>No reviews open.</li>"
            return self.reply(200, "<!doctype html><meta charset=utf-8><title>Plannotator reviews</title>"
                                   "<h1>Reviews</h1><ul>%s</ul>" % items, "text/html; charset=utf-8")
        target = self.route()
        if not target:
            return self.reply(404, "No such review. Open reviews are listed at /\n")
        rid, port, path, needs_slash = target
        if needs_slash:
            return self.reply(308, "", extra=[("Location", "/%s/" % rid)])

        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else None
        headers = {k: v for k, v in self.headers.items()
                   if k.lower() not in HOP and k.lower() not in ("accept-encoding", "content-length")}
        headers["Accept-Encoding"] = "identity"
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=None)
        try:
            conn.request(self.command, path, body=body, headers=headers)
            resp = conn.getresponse()
        except OSError as e:
            conn.close()
            return self.reply(502, "Review %s is not answering on port %d: %s\n" % (rid, port, e))

        ctype = resp.getheader("Content-Type", "")
        is_page = ctype.startswith("text/html") and path.split("?")[0] == "/"
        if is_page:
            page = resp.read().decode("utf-8", "replace")
            shim = SHIM % json.dumps("/" + rid)
            page = re.sub(r"(<head[^>]*>)", lambda m: m.group(1) + shim, page, count=1, flags=re.I)
            conn.close()
            return self.reply(resp.status, page, ctype, [("Referrer-Policy", "same-origin"),
                                                         ("Cache-Control", "no-store")])

        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP and k.lower() != "content-length":
                self.send_header(k, v)
        clen = resp.getheader("Content-Length")
        if clen is not None:
            self.send_header("Content-Length", clen)
        else:
            self.send_header("Connection", "close")
            self.close_connection = True
        self.end_headers()
        try:
            while True:
                chunk = resp.read1(65536) if hasattr(resp, "read1") else resp.read(65536)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
        except (OSError, http.client.HTTPException):
            self.close_connection = True
        finally:
            conn.close()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = handle_any


if __name__ == "__main__":
    os.makedirs(REGISTRY, exist_ok=True)
    srv = ThreadingHTTPServer(("0.0.0.0", LISTEN_PORT), Handler)
    srv.daemon_threads = True
    print("plannotator-router on :%d, registry %s" % (LISTEN_PORT, REGISTRY), file=sys.stderr, flush=True)
    srv.serve_forever()
