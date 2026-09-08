// genroc-lsp: the definition language in an editor. specs/language-server.md.
//
// Its own module for the direction it CAN fence. A language server accumulates editor
// dependencies -- JSON-RPC, incremental parsing, fuzzy matching -- and none of them can reach
// `genroc` or `genctl` from here. It cannot fence the other way: requiring the root module
// takes its dependency graph whole, which is the price of reusing the analysis rather than
// reimplementing it (§4).
//
// Go's internal rule is path-prefix, not module-scoped, so `genroc/lsp` reaches
// `genroc/internal` legitimately -- measured, not assumed. `ui` is fenced because its go.mod
// does not require the root module, which is a different mechanism.
module genroc/lsp

go 1.25.0

require genroc v0.0.0

replace genroc => ../
