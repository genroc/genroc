# bugs/

Reproduced defects, one folder each: a `README.md` and the smallest definitions that show it. A
report says what was run, what came back, what was expected, what is known about the cause, and
the workaround. A limit a spec states on purpose is not a bug: it goes in
[ROADMAP.md](../ROADMAP.md), with the argument in `specs/`.

The definitions are refused on purpose, so the editor marks them. A fixed bug's definitions become
its regression test and the folder goes; git keeps the report.

## Open

- [genctl-ambiguous-error-location](genctl-ambiguous-error-location/README.md) — applying several
  files, an error at an address more than one of them has (the root, a shared task id) names no
  file and no process
