# Paymux public website

Independent Go binary with embedded HTML, CSS and JavaScript. No runtime dependency on the payment hub, database, credentials, CDNs or Node packages.

From the repository root:

```sh
make build-home
./bin/paymux-home -listen 127.0.0.1:8090
```

Use `-listen :8090` to listen on all interfaces behind your reverse proxy. Provide TLS at the proxy. The site serves a homepage, integrations matrix, developer entry guide and self-hosting guide. It has its own `/healthz`. Its `/docs` route is a public guide; the hub's `/docs` remains the operational API reference.

Update capability claims from `docs/capabilities.md` and verification notes from `docs/provider-verification.md` when connectors change. The homepage payment record is explicitly illustrative. No forms collect customer or provider credentials.
