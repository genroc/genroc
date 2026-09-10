# playground

One real definition, driven by hand. `weather-logger` reads open-meteo every ten seconds and
never finishes, so it is what a long-running process and a script task look like when they are
not a test.

`script-node.genroc.yaml` is also a fixture: `tests/integration/script_child_test.ts` reads it.

## Running it

Three terminals. The evaluator is separate because it CLAIMS script tasks off the server and
listens on nothing of its own.

    go run ./cmd/genroc -db tests/playground/genroc.db --http :8888   # the engine
    make script-runner                                               # the evaluator
    export GENROC_SERVER=http://localhost:8888

Then, from this directory — the child first, since the parent names it:

    genctl apply -f script-node.genroc.yaml -f process.genroc.yaml
    genctl run weather-logger --input '{"place":"Praha"}'
    genctl logs <id>          # a snapshot, not a tail -- re-run it to see new readings

Pause it when you are done. The process is infinite, so it outlives the terminal and the next
run would compete with it for the evaluator:

    genctl pause <id>

## The types

`reading.ts` is pulled in by `$import`; `.genroc` registers the resolver that does it, and an
apply typechecks the script against the Input/Output genroc inferred before the string exists.
`genctl types` writes those declarations on their own, which is what an editor wants between
applies:

    genctl types -f script-node.genroc.yaml -f process.genroc.yaml
