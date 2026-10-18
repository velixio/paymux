import base64
import hashlib
import hmac
import sqlite3
import tempfile
import http.client
import socket
import runpy
from unittest.mock import patch
import threading
from http.server import HTTPServer
import unittest
from pathlib import Path
from receiver import Receiver, store_event, verify


class SignedReceiverTests(unittest.TestCase):
    key = b"test-key-for-example-only"
    secret = "whsec_" + base64.b64encode(key).decode()
    body = b'{"type":"payment.succeeded","data":{"object":{"id":"pay_example"}}}'

    def headers(self):
        signature = base64.b64encode(hmac.new(self.key, b"evt_example.1000." + self.body, hashlib.sha256).digest()).decode()
        return {"webhook-id": "evt_example", "webhook-timestamp": "1000", "webhook-signature": "v2,ignored v1,bad! v1," + signature}

    def test_signature_and_timestamp(self):
        self.assertEqual(verify(self.secret, self.headers(), self.body, 1000), "evt_example")
        for secret, headers, body, now in [
            (self.secret, {}, self.body, 1000),
            (self.secret, self.headers(), b"tampered", 1000),
            (self.secret, self.headers(), self.body, 1301),
            (self.secret, self.headers(), self.body, 699),
            ("whsec_!", self.headers(), self.body, 1000),
            ("whsec_", self.headers(), self.body, 1000),
            (self.secret, {**self.headers(), "webhook-timestamp": "bad"}, self.body, 1000),
        ]:
            with self.subTest(headers=headers, now=now), self.assertRaises(ValueError):
                verify(secret, headers, body, now)

    def test_inbox_deduplicates_across_database_connections(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "inbox.db")
            self.assertTrue(store_event(path, "evt_example", self.body))
            self.assertFalse(store_event(path, "evt_example", self.body))
            with sqlite3.connect(path) as db:
                self.assertEqual(db.execute("SELECT body FROM inbox").fetchall(), [(self.body,)])
            for body in [b"not json", b"[]", b'{"type":1,"data":{}}', b'{"type":"payment.succeeded"}']:
                with self.assertRaises(ValueError):
                    store_event(path, "bad", body)


class ReceiverHTTPTests(unittest.TestCase):
    key = SignedReceiverTests.key
    secret = SignedReceiverTests.secret
    body = SignedReceiverTests.body

    def test_signed_delivery_duplicate_and_rejected_requests(self):
        with tempfile.TemporaryDirectory() as directory:
            class LocalReceiver(Receiver):
                secret = self.secret
                inbox_path = str(Path(directory) / "inbox.db")
            server = HTTPServer(("127.0.0.1", 0), LocalReceiver)
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            def post(body, headers):
                connection = http.client.HTTPConnection(*server.server_address, timeout=3)
                connection.request("POST", "/", body=body, headers=headers)
                response = connection.getresponse()
                status = response.status
                response.read(); connection.close()
                return status
            try:
                timestamp = str(int(__import__("time").time()))
                signature = base64.b64encode(hmac.new(self.key, ("evt_example."+timestamp+".").encode()+self.body, hashlib.sha256).digest()).decode()
                headers = {"webhook-id":"evt_example", "webhook-timestamp":timestamp, "webhook-signature":"v1,"+signature}
                self.assertEqual(post(self.body, headers), 204)
                self.assertEqual(post(self.body, headers), 204)
                self.assertEqual(post(b"tampered", headers), 400)
                self.assertEqual(post(b"", headers), 413)
                self.assertEqual(post(self.body, {**headers,"Content-Length":"1048577"}), 413)
                connection = http.client.HTTPConnection(*server.server_address, timeout=3)
                connection.request("POST", "/", body=b"xx", headers={**headers,"Content-Length":"5"})
                connection.sock.shutdown(socket.SHUT_WR)
                response=connection.getresponse()
                self.assertEqual(response.status, 400)
                response.read(); connection.close()
                self.assertEqual(post(self.body, {**headers,"Content-Length":"invalid"}), 400)
                LocalReceiver.inbox_path = str(Path(directory) / "missing" / "inbox.db")
                self.assertEqual(post(self.body, headers), 503)
            finally:
                server.shutdown(); server.server_close(); worker.join(timeout=3)


class ReceiverStartupTests(unittest.TestCase):
    def test_command_reads_configuration_and_binds_only_loopback(self):
        with patch.dict("os.environ", {"PAYMUX_WEBHOOK_SECRET":"whsec_example", "PAYMUX_WEBHOOK_INBOX":"example-inbox.db"}), patch("http.server.HTTPServer") as server:
            runpy.run_path(str(Path(__file__).with_name("receiver.py")), run_name="__main__")
            address, handler=server.call_args.args
            self.assertEqual(address, ("127.0.0.1",9000))
            self.assertEqual(handler.secret,"whsec_example")
            self.assertEqual(handler.inbox_path,"example-inbox.db")
            server.return_value.serve_forever.assert_called_once()


if __name__ == "__main__":
    unittest.main()
