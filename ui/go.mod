// genroc-ui: the web UI and the login that gets a person into it. specs/ui-component.md.
//
// Its own module so that its dependencies are its own. The genroc server links twenty external
// modules -- database drivers, migrations, the expression engine, OpenAPI generation; this links
// two, and CANNOT quietly acquire the others, because they are not reachable from here.
//
// That is the boundary made physical rather than conventional: `ui` may grow (dashboards,
// graphs, whatever the UI becomes) without any of it reaching the binary people embed.
module genroc/ui

go 1.25.0

// One dependency. jwks/ lives inside this module because the SERVER stopped needing it: with
// HS256 there is no key set to fetch there, and only genroc-ui still reads an upstream
// provider's. specs/ui-issued-tokens.md §3.
require github.com/golang-jwt/jwt/v5 v5.3.1

require (
	golang.org/x/crypto v0.55.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
