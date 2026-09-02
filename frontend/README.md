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

## Two ways the credential arrives

Either way it ends up in `localStorage` under one key, so nothing outside `src/api.ts` cares
which happened.

**Behind an SSO proxy**, the app asks `GET /session/token` and gets a bearer token minted from
the identity the proxy forwarded. It asks **only when it holds none**: the server stores just
the hash, so the exchange cannot return a token it issued before and every call mints a new one
— asking on each page load would leave a trail of live credentials. A 401 on a token it already
holds is the ordinary end of a session rather than an error, so it re-exchanges once and
retries; a second failure is real and surfaces. Needs `-auth-config` (`mode: header`) *and*
`-auth token` on the server — the exchange mints a bearer token, so something has to be able to
verify one.

**Without a proxy**, the exchange answers 401 or 501 and the paste field is all there is. This
is the default. A `read` token is enough for everything here:

    genctl token create --perms read --label ui -q

## Scope

Instances list with a status filter, and a detail panel with the stored state. That is the
same question `genctl instances` answers, with a mouse. It polls every 3s rather than holding a
socket — genroc has no change feed, and a poll of one page is cheaper to reason about for a
screen someone watches for a minute. Polling pauses while a detail panel is open.
