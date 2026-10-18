# Implemented Paymux design

These screenshots show the implemented Go dashboard and separate public website, rather than the earlier standalone prototype in the parent directory. Dashboard records are from an isolated simulator database in `/tmp`; no real provider credentials or production data were used.

## Delivered

- Shared responsive dashboard system across the existing operational screens and detail pages, with grouped navigation and a compact phone menu.
- Settings categories, readable policy names, effective values and sources, app overrides and reset, collapsible routing configuration, unsaved-change warning and change-review dialog.
- Refund and other high-impact action review; refund labels, page headings, keyboard focus, skip link and form accessible names.
- Explicit dark-theme palette independent of operating-system preference.
- Independent `web-home/` application: homepage, architecture diagram, provider capability matrix, developer guide and self-hosting guide. No CDN or runtime payment-hub dependency.

## Run

`make build` builds the payment hub. `make build-home` builds `bin/paymux-home`. Start the public site with `./bin/paymux-home -listen 127.0.0.1:8090`; hub and website have separate listeners, configuration and binaries.

## Verification

Chromium checked all 18 dashboard destinations and all four public website pages at 1440, 768, 390 and 360px, with no page-level horizontal overflow. All seven app tabs were checked at desktop and phone widths. Organization settings save, app override and reset, category switching, mobile menu, change review and cancellation, a simulator payment detail and a simulator refund were exercised. Clipboard copying was verified. Primary dashboard routes have headings, skip links and named form controls. Dark-mode settings were rendered.

The dashboard, settings, web and public-website Go packages passed. A broader `go test ./...` run hit `TestBuildLinksAnExtraModule` in `internal/cli`: its generated custom binary failed obtaining VCS status (Git exit 128). This is outside the changed UI code and remains unresolved; the full suite is not reported as passing. No Postgres service was configured for a new live Postgres run.

A formal assistive-technology or WCAG conformance audit was not performed. Public provider capabilities describe connector implementation, with live verification tracked separately in the repository. Nothing was published or deployed externally.
