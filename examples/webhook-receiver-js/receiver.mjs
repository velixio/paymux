// A local signed Paymux webhook receiver with a durable inbox, in JavaScript with
// only Node's built-in modules (Node 18 or newer). It is the JavaScript version of
// examples/webhook-receiver (Python) and examples/webhook-receiver-go.
//
//   PAYMUX_WEBHOOK_SECRET=whsec_... node examples/webhook-receiver-js/receiver.mjs
//
// It binds to loopback, stores verified events (one file per event id, so a
// redelivery is stored once) and does not fulfil orders. Verify the signature over
// the raw request bytes, as here, before you parse or trust anything in the body.
import { createHmac, timingSafeEqual } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { basename, join } from "node:path";
import { pathToFileURL } from "node:url";

const TOLERANCE_SECONDS = 300;

/** Returns the stable event id after the raw-byte signature and age checks, or throws. */
export function verify(secret, headers, body, nowSeconds = Date.now() / 1000) {
  const id = headers["webhook-id"];
  const timestamp = headers["webhook-timestamp"];
  const signatures = headers["webhook-signature"];
  if (!id || !timestamp || !signatures) throw new Error("missing signed headers");
  const seconds = Number(timestamp);
  const encodedKey = secret.replace(/^whsec_/, "");
  const key = Buffer.from(encodedKey, "base64");
  if (!Number.isInteger(seconds) || !/^[A-Za-z0-9+/]+={0,2}$/.test(encodedKey) || key.length === 0) {
    throw new Error("invalid timestamp or secret");
  }
  if (Math.abs(nowSeconds - seconds) > TOLERANCE_SECONDS) throw new Error("timestamp outside tolerance");
  const expected = createHmac("sha256", key).update(`${id}.${seconds}.`).update(body).digest();
  for (const candidate of signatures.split(/\s+/)) {
    const [version, encoded = ""] = candidate.split(",", 2);
    const supplied = Buffer.from(encoded, "base64");
    if (version === "v1" && supplied.length === expected.length && timingSafeEqual(expected, supplied)) return id;
  }
  throw new Error("signature mismatch");
}

/** Stores a verified envelope under its event id, once; false when it is already there. */
export function store(dir, id, body) {
  let envelope;
  try {
    envelope = JSON.parse(body);
  } catch {
    throw new Error("invalid event envelope");
  }
  if (typeof envelope?.type !== "string" || typeof envelope?.data !== "object" || envelope.data === null || Array.isArray(envelope.data)) {
    throw new Error("invalid event envelope");
  }
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  try {
    writeFileSync(join(dir, `${basename(id)}.json`), body, { flag: "wx", mode: 0o600 });
    return true;
  } catch (error) {
    if (error.code === "EEXIST") return false;
    throw error;
  }
}

export function handler(secret, inbox) {
  return (req, res) => {
    const chunks = [];
    let size = 0;
    req.on("data", (chunk) => {
      size += chunk.length;
      if (size <= 1_048_576) chunks.push(chunk);
    });
    req.on("end", () => {
      if (size === 0 || size > 1_048_576) return res.writeHead(413).end();
      const body = Buffer.concat(chunks);
      try {
        store(inbox, verify(secret, req.headers, body), body);
      } catch {
        return res.writeHead(400).end("invalid event or signature");
      }
      res.writeHead(204).end(); // any 2xx tells Paymux it was received
    });
  };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const secret = process.env.PAYMUX_WEBHOOK_SECRET;
  if (!secret) {
    console.error("set PAYMUX_WEBHOOK_SECRET to the endpoint's whsec_ secret");
    process.exit(2);
  }
  createServer(handler(secret, process.env.PAYMUX_WEBHOOK_INBOX ?? "webhook-inbox")).listen(9000, "127.0.0.1", () =>
    console.log("listening on 127.0.0.1:9000"),
  );
}
