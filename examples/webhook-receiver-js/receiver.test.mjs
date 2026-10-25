import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { mkdtempSync, readFileSync, readdirSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { handler, store, verify } from "./receiver.mjs";

const key = Buffer.from("test-key-for-example-only");
const secret = `whsec_${key.toString("base64")}`;
const body = Buffer.from('{"type":"payment.succeeded","data":{"object":{"id":"pay_example"}}}');
const sign = (id, ts, b) => createHmac("sha256", key).update(`${id}.${ts}.`).update(b).digest("base64");
const headers = () => ({
  "webhook-id": "evt_example",
  "webhook-timestamp": "1000",
  "webhook-signature": `v2,ignored v1,bad! v1,${sign("evt_example", 1000, body)}`,
});

test("a signed delivery verifies and returns its event id", () => {
  assert.equal(verify(secret, headers(), body, 1000), "evt_example");
});

test("everything else is refused", () => {
  const cases = [
    [secret, {}, body, 1000],
    [secret, headers(), Buffer.from("tampered"), 1000],
    [secret, headers(), body, 1301],
    [secret, headers(), body, 699],
    ["whsec_!", headers(), body, 1000],
    ["whsec_", headers(), body, 1000],
    [secret, { ...headers(), "webhook-timestamp": "bad" }, body, 1000],
    [`whsec_${Buffer.from("another-key").toString("base64")}`, headers(), body, 1000],
  ];
  for (const [s, h, b, now] of cases) assert.throws(() => verify(s, h, b, now));
});

test("the inbox stores an event once and refuses a malformed envelope", () => {
  const dir = join(mkdtempSync(join(tmpdir(), "inbox-")), "inbox");
  assert.equal(store(dir, "evt_example", body), true);
  assert.equal(store(dir, "evt_example", body), false);
  assert.equal(readFileSync(join(dir, "evt_example.json")).toString(), body.toString());
  for (const bad of ["not json", "[]", '{"type":1,"data":{}}', '{"type":"payment.succeeded"}']) {
    assert.throws(() => store(dir, "bad", Buffer.from(bad)));
  }
});

test("over HTTP: a signed delivery and its redelivery are accepted once, bad ones are 400", async () => {
  const inbox = mkdtempSync(join(tmpdir(), "inbox-"));
  const server = createServer(handler(secret, inbox));
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${server.address().port}/`;
  const now = Math.floor(Date.now() / 1000);
  const post = async (id, ts, signature) =>
    (
      await fetch(url, {
        method: "POST",
        body,
        headers: { "webhook-id": id, "webhook-timestamp": String(ts), "webhook-signature": `v1,${signature}` },
      })
    ).status;
  try {
    assert.equal(await post("evt_1", now, sign("evt_1", now, body)), 204);
    assert.equal(await post("evt_1", now, sign("evt_1", now, body)), 204);
    assert.equal(await post("evt_2", now, "AAAA"), 400);
    assert.equal(await post("evt_3", now - 3600, sign("evt_3", now - 3600, body)), 400);
    assert.deepEqual(readdirSync(inbox), ["evt_1.json"]);
  } finally {
    server.close();
  }
});
