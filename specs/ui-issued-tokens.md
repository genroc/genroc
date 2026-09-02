# genroc-ui issues the token; the server only checks permissions

Status: **PROPOSAL 2026-09-02, nothing built.** Revises [ui-component.md](ui-component.md) §5.1
(which forbade genroc-ui from issuing) and [api-auth.md](api-auth.md) §2.3 (which put the role
map in the server). The two-credential rule of
[auth-two-credentials.md](auth-two-credentials.md) §0 is unchanged.

## 0. The move

genroc-ui stops relaying somebody else's token and starts minting its own:

    browser -> genroc-ui -> OIDC provider, or a password in genroc-ui's config
                         <- identity + GROUPS
               genroc-ui resolves groups -> PERMISSIONS      <- the role map lives here
               genroc-ui mints a short-lived JWT: {sub, perms, exp}, HS256
               cookie in, bearer out
                                       -> genroc verifies the signature and reads `perms`

**The server stops knowing what a group is.** It verifies one issuer and reads a list of
permissions it already understands. Everything about mapping a person to what they may do --
roles, users, group claims, provider quirks -- moves into genroc-ui.

## 1. Why this is not §2.3 being contradicted

api-auth.md §2.3 says a JWT must carry **roles, not permissions**, because "an IdP has no idea
what `deploy` means in genroc, and teaching it would put our authorization model back outside
genroc". That is right about a **third-party** IdP and does not survive contact with an issuer
that is ours: genroc-ui ships with the server, versions with it, and shares its vocabulary by
construction. There is no config in a foreign system to drift.

What §0 of api-auth.md actually protects is unchanged. Its worry was that *which endpoints exist*
would be described somewhere genroc cannot see, going stale as `actions.go` grows. The
**endpoint -> permission** mapping stays exactly where it was, on `actionDef.Allow`. Only the
**group -> permission** mapping moves, and that one was always the deployment's own words.

## 2. The token is a contract, not an internal detail

Because the format is ours, it can be written down -- and then anyone can build a different UI
for genroc without our permission or our code:

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

- `perms` is the resolved set, from the same five in api-auth.md §3. An unrecognised string
  grants nothing -- `Allows` compares against known permissions, so forward compatibility falls
  out rather than needing a rule.
- `sub` is what attribution records: `jwt:ada@example.com`, exactly as today.
- `exp` is short (a minute or so). Nothing long-lived ever leaves genroc-ui.
- Signed **HS256** with a secret shared with the server.

This is the whole integration surface for a third-party UI. It is a smaller and more honest
contract than "implement OIDC and we will map your groups", which is what the server offers now.

## 3. Why HMAC, and what it costs

RSA with a JWKS endpoint is the conventional shape and is rejected for one reason: **the key
would have to persist.** A key regenerated on restart invalidates every session AND poisons the
server's cached JWKS for up to five minutes -- which is not a hypothetical, it is the failure
this repo hit twice in one day through Dex's `storage: memory`. A shared secret has nothing to
generate, nothing to store, and nothing to rotate on restart.

**The cost is real and should be stated rather than discovered.** A symmetric secret means the
server can mint as well as verify, so reading genroc's config yields the ability to forge any
identity, where an RSA public key would not. That is a smaller step down than it looks: anyone
who can read that config can generally reach the database, and a row in `api_tokens` is already
full access (api-auth.md §5.3 makes exactly this argument for `genroc token create`). It would
matter more if the server's config were widely readable, and that is the signal to revisit.

Consequences worth naming:

- The server's jwt mode becomes **HS256 only**. `jwks_url` and `jwks_file` go, and with them the
  RS256/JWKS path -- so `internal/jwks` leaves the root module entirely and lives in `ui/`, which
  still needs it to verify the UPSTREAM provider's token.
- §2.4's pins survive and matter more, not less: `iss`, `aud`, `exp` and a pinned algorithm set
  are what stop a token minted for something else being replayed here. With one algorithm and one
  issuer, the set is trivially pinned.

## 4. Sessions: two tokens, and neither is stored server-side

The cookie and the bearer are deliberately different things.

- **The session cookie** holds a genroc-ui-signed JWT carrying `{sub, groups}` and a longer
  expiry (a working day). `HttpOnly`, `SameSite=Lax`, `Secure`. The upstream provider's ID token
  is used ONCE at login, to establish who this is, and then discarded -- it never reaches a
  cookie and never leaves genroc-ui.
- **The bearer** is minted per request (or cached for a minute) from that session, carrying
  `perms` and a short expiry.

Nothing is stored server-side, so there is no session table and no restart to survive. It also
means the two identity sources converge immediately: OIDC and a config password both produce
`{sub, groups}`, and every step after that is identical.

## 5. Where the role map goes

Out of the server's `auth.yaml` and into genroc-ui's, unchanged in shape:

```yaml
login:
  providers:
    - id: google
      name: Google
      issuer: https://accounts.google.com
      client_id: ...
      client_secret: ...
  passwords:                       # optional; the demo affordance, not a user directory
    - email: ada@example.com
      hash: "$2a$14$..."           # bcrypt, never plaintext
      groups: [genroc-admins]

roles:                             # groups -> permissions. Was the server's; now ours.
  genroc-admins:  [admin]
  "my-org:platform": [deploy, operate, read]
  "*":            [read]
users:                             # subject -> permissions, for providers carrying no groups
  ada@example.com: [admin]

genroc:
  server: http://genroc:8448
  shared_secret: ${GENROC_JWT_SECRET}
  token_ttl: 60s
```

`passwords` is the line that needs watching. It is Dex's `staticPasswords` trade -- one file, no
directory, no registration, no reset -- and it must not grow past that. api-auth.md §9's "no user
directory" applies to the SERVER and stays true; genroc-ui checking a bcrypt hash from a config
file is not a directory, and the moment it wants to be one, the answer is a broker.

## 6. What the server loses

`AuthConfig.Roles`, `AuthConfig.Users`, `grantsFor`, `JWTModeConfig.SubjectClaim`,
`RolesClaim`, `JWKSURL`, `JWKSFile`, and the whole `jwks` package. `JWTAuth` keeps verification
and reads `perms` directly into `[]Grant`.

**Humans lose direct API access**, and that is intended: the server is an API, people reach it
through a UI, and machines use `genroc_sk_*`. A person who wants a script uses a token like any
other machine. The escape hatch is §2's contract -- anything that can mint a conforming JWT is a
first-class client, including a UI somebody else writes.

## 7. Open

- **Does genroc-ui need a `perms` claim namespace?** `perms` is unqualified and could collide if
  a token ever came from elsewhere. `aud: genroc` plus a pinned issuer already scopes it; a
  namespaced claim would be belt and braces.
- **Per-request minting cost.** HMAC signing is microseconds, so caching is probably premature;
  worth measuring before adding a cache with an invalidation story.
- **Multiple providers and the same person.** Two providers can assert the same email. Whether
  that is one identity or two is a policy question this design does not answer.
