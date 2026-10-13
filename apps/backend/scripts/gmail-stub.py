#!/usr/bin/env python3
"""The stand-in for Google Mail the order flow runs against.

The invoice needs a channel that leaves the building, and a mailbox is the one a deployment can offer
without a linked WhatsApp number. This serves the two things that path needs: the consent screen a
connection flow is started against, and the send endpoint an approved draft reaches — plus the profile
read the hub makes when it completes the connection.

What it received is appended to `SMOKE_SENT_LOG` (default `/tmp/smoke-order-gmail-sent.jsonl`), which is
how a test proves that what a person approved is what actually left.

It is one file rather than a heredoc in each script because the smoke and the demo need the same
behaviour, and a second copy is a second place the flow can be wrong.

Usage: python3 scripts/gmail-stub.py <port>
"""
import os

SENT = os.environ.get("SMOKE_SENT_LOG", "/tmp/smoke-order-gmail-sent.jsonl")
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse



class Handler(BaseHTTPRequestHandler):
    def _send(self, payload, status=200):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        path = urlparse(self.path).path

        if path.endswith("/token"):
            form = parse_qs(body.decode())
            if form.get("grant_type", [""])[0] == "refresh_token":
                return self._send({"access_token": "ya29.refreshed", "expires_in": 3600})
            return self._send({"access_token": "ya29.connected", "refresh_token": "1//refresh",
                               "expires_in": 3600, "scope": "https://www.googleapis.com/auth/gmail.send"})

        if "/messages/send" in path:
            with open(SENT, "a") as log:
                log.write(body.decode() + "\n")
            return self._send({"id": "msg-sent", "threadId": "t1"})

        if path.endswith("/drafts"):
            return self._send({"id": "draft-1", "message": {"id": "m1"}})

        return self._send({"error": "unknown"}, 404)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/auth":
            query = parse_qs(parsed.query)
            redirect = query.get("redirect_uri", [""])[0]
            state = query.get("state", [""])[0]
            self.send_response(302)
            self.send_header("Location", "{}?code=a-good-code&state={}".format(redirect, state))
            self.end_headers()
            return

        if parsed.path.endswith("/users/me/profile"):
            return self._send({"emailAddress": "toko@example.com"})

        return self._send({"error": "unknown"}, 404)

    def log_message(self, *args):
        pass


HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
