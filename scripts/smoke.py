#!/usr/bin/env python3
"""Drives a running Paymux, from first-run setup to a refund, with the simulator.

Used by scripts/smoke.sh; it needs only the Python standard library. Every
step either passes or stops the run with a message that says what was
expected and what happened.

    smoke.py --base http://127.0.0.1:8080 --code abcdef-ghijkl-mnopqr-stuvwx
"""
import argparse
import base64
import hashlib
import hmac
import http.cookiejar
import json
import re
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


def fail(msg):
    print("SMOKE FAILED: " + msg, file=sys.stderr, flush=True)
    sys.exit(1)


def step(msg):
    print("==> " + msg, flush=True)


def expect(cond, msg):
    if not cond:
        fail(msg)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Client:
    """A browser-ish client: cookies kept, redirects reported, not followed."""

    def __init__(self, base):
        self.base = base.rstrip("/")
        self.jar = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar), NoRedirect)

    def request(self, method, path, data=None, headers=None):
        """Returns (status, headers, body-bytes)."""
        body = None
        hdr = dict(headers or {})
        if data is not None:
            if isinstance(data, (dict, list)) and hdr.get("Content-Type") == "application/json":
                body = json.dumps(data).encode()
            elif isinstance(data, (dict, list)):
                body = urllib.parse.urlencode(data, doseq=True).encode()
                hdr.setdefault("Content-Type", "application/x-www-form-urlencoded")
            else:
                body = data
        req = urllib.request.Request(self.base + path, data=body, method=method, headers=hdr)
        try:
            with self.opener.open(req, timeout=30) as res:
                return res.status, res.headers, res.read()
        except urllib.error.HTTPError as e:
            return e.code, e.headers, e.read()

    def get(self, path, **kw):
        return self.request("GET", path, **kw)

    def post(self, path, data, **kw):
        return self.request("POST", path, data=data, **kw)


class Receiver(BaseHTTPRequestHandler):
    """The app's webhook endpoint."""

    events = []
    lock = threading.Lock()

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with Receiver.lock:
            Receiver.events.append((dict(self.headers), body))
        self.send_response(200)
        self.end_headers()

    def log_message(self, *a):
        pass


def verify_signature(secret, headers, body):
    """Standard Webhooks: HMAC-SHA256 of "id.timestamp.body" with the whsec_ key."""
    h = {k.lower(): v for k, v in headers.items()}
    key = base64.b64decode(secret.removeprefix("whsec_"))
    signed = f"{h['webhook-id']}.{h['webhook-timestamp']}.".encode() + body
    want = base64.b64encode(hmac.new(key, signed, hashlib.sha256).digest()).decode()
    return any(s.split(",", 1)[-1] == want for s in h["webhook-signature"].split())


def wait_for(what, fn, seconds=30):
    deadline = time.time() + seconds
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(0.3)
    fail(f"timed out after {seconds}s waiting for {what}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--code", required=True, help="the first-run setup code from the server log")
    args = ap.parse_args()
    c = Client(args.base)

    # The app's webhook receiver.
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    receiver_url = f"http://127.0.0.1:{srv.server_address[1]}/hooks"

    def csrf_of(html):
        m = re.search(r'name="csrf" value="([^"]+)"', html)
        expect(m, "no csrf field in the page")
        return m.group(1)

    def dashboard_post(path, fields, page=None, status=(200, 303)):
        """POST a dashboard form, taking the csrf token from a page of the same session."""
        st, _, body = c.get(page or "/dashboard/apps")
        expect(st == 200, f"GET {page or '/dashboard/apps'}: {st}")
        fields = dict(fields, csrf=csrf_of(body.decode()))
        st, hdr, out = c.post(path, fields)
        expect(st in status, f"POST {path}: {st}\n{out.decode(errors='replace')[:600]}")
        return st, hdr, out.decode(errors="replace")

    step("first-run setup")
    st, _, body = c.get("/setup")
    expect(st == 200, f"GET /setup: {st} (is this a fresh data directory?)")
    st, _, body = c.post("/setup", {
        "csrf": csrf_of(body.decode()), "token": args.code, "email": "admin@example.com", "password": "correct horse battery",
        "password2": "correct horse battery", "org_name": "Smoke Org", "app_name": "Smoke Shop", "app_slug": "smoke-shop"})
    html = body.decode()
    expect(st == 200 and "Setup complete" in html, f"POST /setup: {st}\n{html[:500]}")
    m = re.search(r"(pmx_test_sk_[A-Za-z0-9_\-]+)", html)
    expect(m, "the setup page did not show a test API key")
    api_key = m.group(1)
    st, _, _ = c.get("/setup")
    expect(st == 404, f"/setup should close after use, got {st}")

    step("find the app")
    st, _, body = c.get("/dashboard/apps")
    m = re.search(r"/dashboard/apps/(app_[0-9a-z]+)", body.decode())
    expect(st == 200 and m, "the app is not listed in the dashboard")
    app_id = m.group(1)

    step("GST profile, so that invoices are issued")
    _, hdr, _ = dashboard_post("/dashboard/tax-entities", {
        "legal_name": "Smoke Shop Private Limited", "gstin": "27AAPFU0939F1ZV", "address": "1 Marine Drive, Mumbai 400001",
        "signatory_name": "A Director"}, page="/dashboard/tax-entities")
    entity = re.search(r"/dashboard/tax-entities/(\S+)", hdr.get("Location", ""))
    expect(entity, f"the tax entity was not created (Location {hdr.get('Location')!r})")
    dashboard_post(f"/dashboard/apps/{app_id}/tax", {
        "tax_entity_id": entity.group(1), "issue_invoices": "1", "gst_rate": "18", "prices_include_tax": "1", "sac_code": "998315"},
        page=f"/dashboard/apps/{app_id}")

    step("a simulator provider account, linked to the app")
    _, hdr, _ = dashboard_post("/dashboard/providers", {
        "connector": "simulator", "name": "Simulator", "mode": "test", "app_id": app_id,
        "currency": ["INR", "USD", "BDT"], "purpose": ["one_time", "subscription"]}, page="/dashboard/providers/new?connector=simulator")
    expect("/dashboard/providers/pa_" in hdr.get("Location", ""), f"the simulator account was not created (Location {hdr.get('Location')!r})")

    step("a webhook endpoint for the app")
    _, _, html = dashboard_post(f"/dashboard/apps/{app_id}/endpoints", {"url": receiver_url}, page=f"/dashboard/apps/{app_id}")
    m = re.search(r'id="revealed-secret">(whsec_[^<]+)<', html)
    expect(m, "the endpoint's signing secret was not shown")
    secret = m.group(1)

    api = Client(args.base)  # no cookies: API key only

    def call(method, path, body=None, expect_status=None):
        hdr = {"Authorization": "Bearer " + api_key}
        if body is not None:
            hdr["Content-Type"] = "application/json"
        st, h, out = api.request(method, path, data=body, headers=hdr)
        if expect_status is not None:
            expect(st == expect_status, f"{method} {path}: {st} (wanted {expect_status})\n{out.decode(errors='replace')[:500]}")
        return st, h, out

    def create_payment(amount):
        _, _, out = call("POST", "/v1/payments", {
            "amount": amount, "currency": "INR", "order_ref": "smoke-" + str(amount), "description": "Smoke test",
            "return_url": "https://shop.example.com/thanks",
            "customer": {"external_ref": "smoke-user", "name": "Asha Rao", "email": "asha@example.com",
                         "billing": {"state_code": "29", "country": "IN"}}}, expect_status=201)
        p = json.loads(out)
        return p["id"], urllib.parse.urlsplit(p["checkout_url"])

    def payment(pid):
        return json.loads(call("GET", "/v1/payments/" + pid, expect_status=200)[2])

    step("create a payment through the API")
    pid, checkout = create_payment(49900)
    expect(checkout.path == "/pay/" + pid, f"checkout_url {checkout}")

    step("the pay page sends the customer to the simulator")
    st, hdr, body = c.get(checkout.path + "?" + checkout.query)
    loc = hdr.get("Location", "")
    m = re.match(r"^https?://[^/]+(/sim/[0-9A-Za-z]+\?pa=pa_[0-9a-z]+)$", loc)
    expect(st == 303 and m, f"pay page: {st} Location {loc!r}\n{body.decode(errors='replace')[:400]}")
    sim_page = m.group(1)
    st, _, body = c.get(sim_page)
    expect(st == 200 and b"Succeed" in body and b"499.00" in body, f"simulator page: {st}\n{body.decode(errors='replace')[:400]}")

    step("click Succeed; the return goes through /return and ends at the shop")
    st, hdr, _ = c.post(sim_page, {"action": "succeed"})
    expect(st == 303 and hdr.get("Location", "").startswith("/return/"), f"decision: {st} {hdr.get('Location')!r}")
    st, hdr, _ = c.get(hdr["Location"])
    back = urllib.parse.urlsplit(hdr.get("Location", ""))
    expect(st == 303 and back.netloc == "shop.example.com" and "status=succeeded" in back.query, f"return: {st} {hdr.get('Location')!r}")
    p = payment(pid)
    expect(p["status"] == "succeeded" and p["amount_received"] == 49900, f"payment after return: {p}")
    expect(p["attempts"] and p["attempts"][0]["provider"] == "simulator" and p["attempts"][0]["status"] == "captured", f"attempts: {p.get('attempts')}")

    step("the app's webhook endpoint received a correctly signed payment.succeeded")

    def event(kind):
        with Receiver.lock:
            for headers, body in Receiver.events:
                if json.loads(body).get("type") == kind:
                    return headers, body
        return None

    headers, body = wait_for("payment.succeeded at the webhook receiver", lambda: event("payment.succeeded"))
    expect(verify_signature(secret, headers, body), "the payment.succeeded webhook signature does not verify")
    obj = json.loads(body)["data"]["object"]
    expect(obj["id"] == pid and obj["status"] == "succeeded", f"webhook object: {obj}")

    step("the invoice PDF")
    inv = wait_for("the invoice", lambda: json.loads(call("GET", "/v1/invoices?payment_id=" + pid, expect_status=200)[2])["data"])
    expect(len(inv) == 1 and inv[0]["total"] == 49900, f"invoices: {inv}")

    def fetch_pdf():
        st, _, out = call("GET", "/v1/invoices/" + inv[0]["id"] + "/pdf")
        return out if st == 200 else None  # rendered by a job, so it may take a moment

    pdf = wait_for("the invoice PDF", fetch_pdf)
    expect(pdf.startswith(b"%PDF") and len(pdf) > 2000, f"not a PDF: {pdf[:20]!r} ({len(pdf)} bytes)")

    step("a partial refund")
    _, _, out = call("POST", f"/v1/payments/{pid}/refunds", {"amount": 10000, "reason": "smoke test"}, expect_status=201)
    rid = json.loads(out)["id"]
    r = wait_for("the refund to succeed", lambda: (lambda j: j if j["status"] == "succeeded" else None)(json.loads(call("GET", "/v1/refunds/" + rid, expect_status=200)[2])))
    expect(r.get("provider_refund_id", "").startswith("sim_rfnd_"), f"refund: {r}")
    headers, body = wait_for("refund.succeeded at the webhook receiver", lambda: event("refund.succeeded"))
    expect(verify_signature(secret, headers, body), "the refund.succeeded webhook signature does not verify")
    expect(payment(pid)["amount_refunded"] == 10000, "the payment does not show the refund")
    notes = wait_for("the credit note", lambda: json.loads(call("GET", "/v1/credit_notes?payment_id=" + pid, expect_status=200)[2])["data"])
    expect(len(notes) == 1, f"credit notes: {notes}")

    step("a refund made in the provider's own dashboard is booked as a provider refund")
    st, _, page = c.get("/dashboard/payments/" + pid)
    expect(st == 200 and b"Refund from the provider" in page, f"the payment page has no provider-refund button: {st}")
    html = page.decode()
    att = re.search(r'<option value="(att_[0-9a-z]+)"', html)
    expect(att, "the payment page lists no simulator attempt")
    st, _, out = c.post(f"/dashboard/payments/{pid}/simulate-refund", {"csrf": csrf_of(html), "attempt_id": att.group(1), "amount": "50.00"})
    expect(st == 200 and b"Refund made at the simulated provider" in out, f"simulate-refund: {st}\n{out.decode(errors='replace')[:400]}")
    wait_for("the provider refund to be booked", lambda: payment(pid)["amount_refunded"] == 15000)

    def provider_refund_event():
        with Receiver.lock:
            for headers, body in Receiver.events:
                j = json.loads(body)
                if j.get("type") == "refund.succeeded" and j["data"]["object"].get("initiated_by") == "provider":
                    return headers, body
        return None

    headers, body = wait_for("refund.succeeded (initiated_by=provider) at the webhook receiver", provider_refund_event)
    expect(verify_signature(secret, headers, body), "the provider refund webhook signature does not verify")
    obj = json.loads(body)["data"]["object"]
    expect(obj["amount"] == 5000 and obj["provider_refund_id"].startswith("sim_rfnd_"), f"provider refund object: {obj}")
    notes = wait_for("the second credit note", lambda: (lambda d: d if len(d) == 2 else None)(json.loads(call("GET", "/v1/credit_notes?payment_id=" + pid, expect_status=200)[2])["data"]))
    expect(len(notes) == 2, f"credit notes after a provider refund: {notes}")

    step("pending, then success by webhook alone")
    pid2, checkout2 = create_payment(25000)
    _, hdr, _ = c.get(checkout2.path + "?" + checkout2.query)
    page2 = re.match(r"^https?://[^/]+(/sim/\S+)$", hdr["Location"]).group(1)
    st, hdr, _ = c.post(page2, {"action": "pending_succeed", "delay": "2"})
    expect(st == 303, f"pending decision: {st}")
    c.get(hdr["Location"])
    wait_for("the delayed success to arrive by webhook", lambda: payment(pid2)["status"] == "succeeded")

    step("a failed payment")
    pid3, checkout3 = create_payment(1000)
    _, hdr, _ = c.get(checkout3.path + "?" + checkout3.query)
    page3 = re.match(r"^https?://[^/]+(/sim/\S+)$", hdr["Location"]).group(1)
    st, hdr, _ = c.post(page3, {"action": "fail"})
    c.get(hdr["Location"])
    p3 = wait_for("the failed attempt", lambda: (lambda p: p if p["attempts"] and p["attempts"][0]["status"] == "failed" else None)(payment(pid3)))
    expect(p3["status"] != "succeeded" and p3["attempts"][0]["failure_code"] == "simulated_failure", f"failed payment: {p3}")

    print("SMOKE OK", flush=True)


if __name__ == "__main__":
    main()
