"""Local signed Paymux receiver with a durable inbox; Python 3.9+ standard library.

PAYMUX_WEBHOOK_SECRET=whsec_... python3 examples/webhook-receiver/receiver.py
This binds to loopback, stores verified events, and does not fulfill orders.
"""
from contextlib import closing
import base64
import hashlib
import hmac
import json
import os
import sqlite3
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


def verify(secret, headers, body, now=None):
    """Return the stable event ID after raw-byte signature and age checks."""
    event_id = headers.get("webhook-id", "")
    timestamp = headers.get("webhook-timestamp", "")
    signatures = headers.get("webhook-signature", "")
    if not event_id or not timestamp or not signatures:
        raise ValueError("missing signed headers")
    try:
        seconds = int(timestamp)
        key = base64.b64decode(secret.removeprefix("whsec_"), validate=True)
    except (ValueError, TypeError) as error:
        raise ValueError("invalid timestamp or secret") from error
    if not key or abs((time.time() if now is None else now) - seconds) > 300:
        raise ValueError("empty key or timestamp outside tolerance")
    expected = hmac.new(key, f"{event_id}.{seconds}.".encode() + body, hashlib.sha256).digest()
    for candidate in signatures.split():
        version, _, encoded = candidate.partition(",")
        if version != "v1":
            continue
        try:
            supplied = base64.b64decode(encoded, validate=True)
        except ValueError:
            continue
        if hmac.compare_digest(expected, supplied):
            return event_id
    raise ValueError("signature mismatch")


def store_event(path, event_id, body):
    """Persist one verified envelope; one inbox file belongs to one endpoint."""
    envelope = json.loads(body)
    if not isinstance(envelope, dict) or not isinstance(envelope.get("type"), str) or not isinstance(envelope.get("data"), dict):
        raise ValueError("invalid event envelope")
    with closing(sqlite3.connect(path)) as db, db:
        db.execute("CREATE TABLE IF NOT EXISTS inbox (event_id TEXT PRIMARY KEY, body BLOB NOT NULL)")
        result = db.execute("INSERT OR IGNORE INTO inbox VALUES (?, ?)", (event_id, body))
        return result.rowcount == 1


class Receiver(BaseHTTPRequestHandler):
    secret = ""
    inbox_path = "webhook-inbox.db"

    def do_POST(self):
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 < length <= 1_048_576:
                self.send_error(413)
                return
            body = self.rfile.read(length)
            if len(body) != length:
                raise ValueError("truncated body")
            event_id = verify(self.secret, self.headers, body)
            store_event(self.inbox_path, event_id, body)
        except ValueError:
            self.send_error(400, "invalid event or signature")
            return
        except sqlite3.Error:
            self.send_error(503, "inbox unavailable")
            return
        self.send_response(204)
        self.end_headers()

    def log_message(self, *_args):
        pass  # Do not log request bodies, customer data, or secrets.


if __name__ == "__main__":
    Receiver.secret = os.environ["PAYMUX_WEBHOOK_SECRET"]
    Receiver.inbox_path = os.environ.get("PAYMUX_WEBHOOK_INBOX", "webhook-inbox.db")
    HTTPServer(("127.0.0.1", 9000), Receiver).serve_forever()
