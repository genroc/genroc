# genroc with a login through Dex

genroc-ui signs people in with any OIDC provider. Dex is one that fronts the rest — GitHub,
LDAP, SAML, Google Workspace groups — and here its own password database stands in for them, so
the stack runs with no credentials of your own.

    docker compose -f examples/dex/compose.yaml up --build
    open http://localhost:8448                      # alice@example.com / demo

| user | password | permissions (`ui.yaml`) |
|---|---|---|
| `alice@example.com` | `demo` | `admin`, by email under `users:` |
| `bob@example.com` | `demo` | `read`, from the `"*"` role |

Delete `./data` to start over: it holds genroc's database, the signing key genroc and genroc-ui
share, and Dex's state.

## Two addresses for one Dex

The browser is sent to Dex at `localhost:5556`; genroc-ui, inside the compose network, reaches it
at `dex:5556`. So `ui.yaml` names the browser's address as `issuer` — the value Dex's tokens
carry, and `dex.yaml`'s `issuer` exactly — and overrides `discovery_url`, `token_url` and
`jwks_url` with the internal one. Get the issuer wrong and genroc-ui refuses to start.

## A real directory

Uncomment the GitHub connector in `dex.yaml`, register `http://localhost:5556/dex/callback` as
the GitHub OAuth app's callback, and map its teams in `ui.yaml`:

```yaml
roles:
  my-org:platform: [admin]
```

genroc-ui's side does not change: it still sees one OIDC provider.

## Connecting genctl

genroc is not published; genctl goes through genroc-ui with a token of its own. Sign in as
alice, mint one in the tokens tab, then:

    genctl config set token genroc_sk_...
    genctl instances
