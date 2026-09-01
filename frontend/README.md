# genroc UI

A small React app for watching instances. Vite dev server, no styling framework.

    cd frontend && npm install && npm run dev      # http://localhost:5173

It expects genroc on `http://localhost:8448`; override with `GENROC_SERVER=… npm run dev`.

## The proxy is why there is no CORS anywhere

`vite.config.ts` proxies `/api` and `/healthz` to genroc, so **the browser only ever talks to
one origin** and the hop to genroc is server-to-server. genroc needs no `Access-Control-*`
headers and this app needs no base URL — `fetch("/api/instances")` is a relative path in both
dev and production, because in production genroc serves these assets itself at `/`
(specs/api-auth.md §5.1).

That is worth preserving. The moment this app is served from an origin genroc does not own,
CORS becomes a thing genroc has to configure and get right.

## The credential is pasted, for now

genroc has no browser login: `jwt` and `header` modes are unbuilt, so there is nothing that
turns a session into a token. The header field takes a `genroc_sk_…` value and keeps it in
`localStorage`. A `read` token is enough for everything here:

    genctl token create --perms read --label ui -q

When a session exchange exists it replaces `token()` in `src/api.ts`, and nothing else changes.

## Scope

Instances list with a status filter, and a detail panel with the stored state. That is the
same question `genctl instances` answers, with a mouse. It polls every 3s rather than holding a
socket — genroc has no change feed, and a poll of one page is cheaper to reason about for a
screen someone watches for a minute. Polling pauses while a detail panel is open.
