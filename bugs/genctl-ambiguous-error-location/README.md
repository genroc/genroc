# genctl loses which file failed when an address is in more than one

Seen with genctl 0.31.0 (c30783fb), server `ghcr.io/genroc/genroc:0.31.0`.

## Reproduce

    genctl apply --check-only -f bugs/genctl-ambiguous-error-location/ok.genroc.yaml bugs/genctl-ambiguous-error-location/orphan.genroc.yaml
    genctl apply --check-only -f bugs/genctl-ambiguous-error-location/ok.genroc.yaml bugs/genctl-ambiguous-error-location/shared-task.genroc.yaml

`ok` is valid. `orphan` is refused at the document root (a task nothing routes to), `shared-task`
at `tasks.a.action.for`, an address `ok` has too, since both have a task `a`.

- **Expected:** each error names the file it is in, as it does when that file is applied alone:
  `orphan.genroc.yaml:2:1: task "orphan" is unreachable: …` and
  `shared-task.genroc.yaml:5:33: task "a" delay for: …`.
- **Actual:**

      genctl: : task "orphan" is unreachable: no switch or error handler routes to it
      genctl: tasks.a.action.for: task "a" delay for: duration "x": expected a number before "x"

  Neither line names a file or a process, and the first prints an empty address.

A plain `genctl apply` sends every file `.genroc`'s `definitions:` names, so in a repository with
more than one definition every root-level error, and every error under an address two files share
(`name`, `input_schema`, a common task id), comes out like this.

## What is known

- **genctl drops the location on purpose when it is ambiguous.** `fatalLocated` looks each
  `fields[].field` up in every document (`locate`, cmd/genctl/expand.go). When more than one claims
  it, it does not guess ("pointing at either would be a guess") and walks to the parent address.
  The root `""` is in every document, so the walk ends unlocated, and the fallback prints
  `genctl: <field>: <message>` with the field empty.
- **The server knows which definition failed, but only in prose.** The validate answer for the
  first pair is

      {"error": "bug-located-orphan: task \"orphan\" is unreachable: …", "code": "invalid",
       "fields": [{"field": "", "rule": "def.structure", "message": "task \"orphan\" is unreachable: …"}]}

  The top-level `error` starts with the definition's name, but a `fields` entry carries only a
  path within some definition. `locate` matches on the address, never on prose, so it has nothing
  to tell the documents apart.
- A single file is located correctly, and so is a multi-file error whose address only one
  document has. The language server works on one document, so the editor is not affected.

The likely fix is on the server's side of the contract: each `fields` entry names its definition
(its `name`, or its index in the request), and `locate` searches only that document. Parsing the
name back out of `error` would break genctl's no-prose rule.

## Workaround

Apply the files one at a time to find the one that fails.
