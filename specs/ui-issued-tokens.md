# genroc-ui issues the token; the server only checks permissions

Status: **Built.**

## 0. The move

genroc-ui mints its own token rather than relaying a provider's:

    browser -> genroc-ui -> OIDC provider, or a password in genroc-ui's config
                         <- identity + GROUPS
               genroc-ui resolves groups -> PERMISSIONS      <- the role map lives here
               genroc-ui mints a short-lived JWT: {sub, perms, exp}, HS256
               cookie in, bearer out
                                       -> genroc verifies the signature and reads `perms`

**The server never learns what a group is.** It verifies one issuer and reads permissions it
already understands; roles, users, group claims and provider quirks all live in genroc-ui. A
provider is trusted for exactly two things, who someone is and their groups (`oidc.Claims`).

## 1. Why the token carries permissions, not groups

A third-party IdP's token should carry groups: the IdP has no idea what `deploy` means, and
teaching it would put genroc's authorization model in a foreign config. genroc-ui is not third
party — it ships and versions with the server and shares its vocabulary by construction. What
api-auth.md §0 protects, the **endpoint → permission** mapping, stays on `actionDef.Allow`; only
**group → permission**, always the deployment's own words, moves here.

## 2. The token is a contract, not an internal detail

```json
{
  "iss": "genroc-ui",
  "aud": "genroc",
  "sub": "ada@example.com",
  "perms": ["deploy", "operate", "read"],
  "iat": 1788000000,
  "exp": 1788000060
}
```

- `perms` is the resolved set, from the five in api-auth.md §3. An unrecognised string grants
  nothing and breaks nothing (`Allows` never matches it), so a newer issuer degrades rather than
  fails.
- `sub` is what attribution records: `jwt:ada@example.com`.
- `exp` is short (`token.ttl`, default 60s) and required by the server.
- `iss` and `aud` default to these values on both sides; set one and you must set it on both.

Anything that mints a conforming token is a first-class client, including a UI somebody else
writes. **People reach the API only through such an issuer**, deliberately: a person who wants a
script uses a `genroc_sk_*` like any machine.

## 3. Why HMAC, and what it costs

Not RSA with a JWKS endpoint: **the key would have to persist.** One regenerated on restart
invalidates every session and poisons the server's cached key set — the failure this repo hit
twice through Dex's `storage: memory`. A shared secret has nothing to generate, store, or rotate
on restart.

**The cost:** the server can mint as well as verify, so reading its config yields the ability to
forge any identity. That is a short step from database access, which is already full access
(api-auth.md §5.3). When that stops holding is §7.

So the server's jwt mode is HS256 only, and the `jwks` package lives in `ui/`, which still
verifies upstream providers' ID tokens. Both sides refuse a secret under 32 characters.

## 4. Sessions: two tokens, and neither is stored server-side

- **The session cookie** (`genroc_session`) holds a genroc-ui-signed JWT `{sub, groups}` with
  `aud: genroc-ui-session` — so it cannot be replayed as an access token under the same key — and
  expires after `session_ttl` (default 12h). `HttpOnly`, `SameSite=Lax`. The provider's ID token is
  used once at login and discarded.
- **The access token** is minted from that session on every proxied request, carrying `perms`.
  Not cached: verify, resolve and sign cost ~6–8 µs on an M1 (`ui/token_bench_test.go`) against
  milliseconds of proxying, and minting at use is what makes a role-map edit take effect on the
  next request. Not a second cookie: the access half never needs to reach the browser.

OIDC and a config password both produce `{sub, groups}`, and every step after is identical. With
no session table there is nothing to survive a restart.

**Groups are captured at login**, so a change at the provider is invisible until the next login,
while a role-map edit takes effect on the next request. There is no refresh clock: only the
provider knows a person's groups, so re-deriving them means a redirect every few minutes, and
`session_ttl` would stop bounding a session once each refresh minted a fresh one;
`offline_access` avoids the redirect, but Dex rotates refresh tokens, so concurrent requests race
for the one valid copy. `POST /auth/logout` — sign out, sign in — is the lever (§7).

**Revoking everyone** needs nothing built: rotate the shared secret and restart both components
together, and every cookie and outstanding access token dies at once — both, because genroc-ui
alone would mint tokens the server rejects. All-or-nothing, so break-glass rather than routine.
`genroc_sk_*` machine tokens are hashed in `api_tokens` and untouched.

## 5. Where the role map goes

genroc-ui's config file (`--config` / `$GENROC_UI_CONFIG`) — a file, not flags, because providers,
roles and users are lists and maps:

```yaml
server: http://genroc:8448
login:
  providers:
    - id: google
      type: google                 # fills issuer and scopes; groups fetched from Workspace
      client_id: ...
      client_secret: ...
  passwords:                       # optional; the demo affordance, not a user directory
    - email: ada@example.com
      hash: "$2a$14$..."           # bcrypt; plaintext is refused
      groups: [genroc-admins]
roles:                             # groups -> permissions; "*" is anyone signed in
  genroc-admins:  [admin]
  "*":            [read]
users:                             # subject -> permissions, for providers carrying no groups
  ada@example.com: [admin]
token:
  secret_file: /data/jwt-secret    # or `secret:`; the key the server verifies with
```

A person's permissions are the union of their groups' roles, their `users` entry, and `"*"`. Any
other provider is a generic `oidc` entry with an `issuer`; discovery runs at startup, so an
unreachable provider fails there rather than at someone's first login. Without a config,
`--server` alone runs a UI with no login.

The login page is its own bundle (`login.html`), not a route inside the app it gates, because it
renders before any session exists; it asks `GET /auth/options` which ways in exist and posts to
`POST /auth/password`.

`passwords` is the line that needs watching. It is Dex's `staticPasswords` trade — one file, no
directory, no registration, no reset — and it must not grow past that. genroc-ui checking a bcrypt
hash from its config is not a user directory; the moment it wants to be one, the answer is a
broker.

## 7. Open

- **A config-readable forging key (§3)** — revisit HMAC if the server's config becomes widely
  readable.
- **Group refresh** — once groups can be re-fetched *without* a redirect; until then, logout.
- **Revocation by subject** — a list genroc-ui refuses sessions against, re-running the login for
  one person: one operator action, where a refresh clock spends a round trip on every session.
- **Per-request minting under RSA (§4)** — signing goes from ~2 µs to ~1 ms, and at 400x minting
  at use stops being free.
- **Two providers asserting the same email** — one identity or two is a policy question this
  design does not answer; it needs one once a deployment's providers overlap.
