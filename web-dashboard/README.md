# Paymux React dashboard

Frontend-only React application using TanStack Router, TanStack Query, Tailwind CSS and the shared shadcn/ui components in `frontend/components/ui`. It adds no API, database, server functions or separate authentication system.

## Existing backend contract

The Go application already implements administration as authenticated HTML pages and CSRF-protected forms. `src/adapter.ts` converts those existing responses into a typed presentation model. React renders the model with shadcn controls and TanStack navigation/query state. Mutations submit the original action, method, field names, repeated values, clicked button value and upload encoding. Validation errors retain entered values. Downloads remain native browser links.

This adapter intentionally depends on the Go view contract. Keep its tests and browser workflow checks when changing templates. It does not scrape provider dashboards or store provider API credentials. Query data is not persisted; inactive page queries are discarded, and mode changes/logout clear cached data. A one-time key response stays only in the current view.

The original Go view is available until React mounts and remains a no-JavaScript fallback. `?classic=1` opens the classic view. Custom template replacements that remove the standard view structure should retain the classic experience or adopt the presentation contract.

## Develop

Install Bun 1.4.3 or newer. From the repository root:

```sh
bun install --frozen-lockfile
bun run dev
```

Run the existing Go hub separately at `http://localhost:18080` (adjust the proxy in `vite.config.ts` for another address). Open `http://localhost:5173/dashboard/overview`. Vite serves only the React shell and assets; requests for page data, sign-in and form actions are proxied to the existing hub. No development API is introduced.

## Build and check

```sh
bun run typecheck
bun run test:frontend
make build
```

`make build` compiles the dashboard frontend before embedding its assets into `bin/paymux`. Node or Bun is not required at runtime. Direct `go build` without the frontend build retains the server-rendered fallback but does not include the React UI.

Shared UI components are installed from the official shadcn registry. Add components with `bunx shadcn@latest add COMPONENT`. Vite builds client assets only; TanStack Start and server-side rendering are not used.
