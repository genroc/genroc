import { expect, test } from "vitest";
import { client, startMockService, waitForInstance } from "../helpers/client.ts";

async function defineWith(query: unknown, urlSuffix = "/search") {
  const svc = await startMockService(0, { response: { ok: true } });
  const name = `query_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { term: { type: "string" }, page: { type: ["integer", "null"] } },
        required: ["term", "page"],
      },
      tasks: [
        {
          id: "call",
          action: {
            type: "fetch" as const,
            url: `http://localhost:${svc.port}${urlSuffix}`,
            method: "GET",
            query,
            responses: { 200: { type: "object", properties: { ok: { type: "boolean" } } } },
          },
          timeout: 2000,
          switch: [{ goto: "end" }],
        },
      ],
    } as any,
  });
  expect(putErr).toBeUndefined();
  return { svc, name };
}

async function run(name: string, input: unknown) {
  const { data } = await client.POST("/instances", { body: { process: name, input } as any });
  expect(await waitForInstance(data!.id)).toBe("completed");
}

// The reason the slot exists. Interpolating a term into the url escapes nothing, so a value
// carrying `&`, `=`, `#` or a space corrupts the url or injects a parameter — reachable from
// untrusted process input, which makes it a bug class rather than an ergonomic complaint.
test("query — values are URL-encoded, so a term cannot inject a parameter", async () => {
  const { svc, name } = await defineWith({ q: "$: input.term" });
  const term = "a&admin=1 b#c=d";

  await run(name, { term, page: null });

  const requested = svc.requestUrls()[0];
  const parsed = new URL(`http://x${requested}`);
  // One parameter, carrying the term verbatim — not several, and not truncated at the `#`.
  expect([...parsed.searchParams.keys()]).toEqual(["q"]);
  expect(parsed.searchParams.get("q")).toBe(term);
  expect(requested).not.toContain("admin=1");

  await svc.stop();
});

// A null omits its parameter, which is what saves an optional one from a conditional — and is
// deliberately unlike headers, where a null is an error.
test("query — a null value omits its parameter, a present one is sent", async () => {
  const { svc, name } = await defineWith({ q: "$: input.term", page: "$: input.page" });

  await run(name, { term: "hello", page: null });
  expect([...new URL(`http://x${svc.requestUrls()[0]}`).searchParams.keys()]).toEqual(["q"]);

  // A number renders without the author stringifying it — which `${ }` could not do here,
  // since interpolating a nullable is refused at registration.
  await run(name, { term: "hello", page: 3 });
  expect(new URL(`http://x${svc.requestUrls()[1]}`).searchParams.get("page")).toBe("3");

  await svc.stop();
});

// Appended, not exclusive: a url may already carry its own parameters.
test("query — appends to a url that already has a query string", async () => {
  const { svc, name } = await defineWith({ q: "$: input.term" }, "/search?fixed=1");

  await run(name, { term: "x", page: null });

  const parsed = new URL(`http://x${svc.requestUrls()[0]}`);
  expect(parsed.searchParams.get("fixed")).toBe("1");
  expect(parsed.searchParams.get("q")).toBe("x");

  await svc.stop();
});

// What genroc puts on the wire, character by character. The table is the contract: every
// character that could end the value, start another parameter, or truncate the url is
// percent-encoded, and each one decodes back to exactly what was sent.
//
// Space is `%20`, not the `+` that url.Values.Encode emits. Every mainstream decoder reads
// `+` back as a space, but RFC 3986 says a query is just a string and `+` is a literal plus —
// a server reading it that way takes the wrong value SILENTLY. `%20` is a space under both
// readings, so it is safe where `+` is merely usually safe.
test("query — encoding is exact for every character that could break a url", async () => {
  const { svc, name } = await defineWith({ p: "$: input.term" });

  const cases: [string, string][] = [
    ["a b", "a%20b"],       // not `+`: unambiguous under RFC 3986 too
    ["a+b", "a%2Bb"],       // a literal plus survives as itself
    ["a&b", "a%26b"],       // would otherwise start a second parameter
    ["a=b", "a%3Db"],       // would otherwise end the name
    ["a#b", "a%23b"],       // would otherwise truncate the url at the fragment
    ["a?b", "a%3Fb"],
    ["café", "caf%C3%A9"],  // UTF-8, percent-encoded per byte
  ];

  for (const [sent] of cases) {
    await run(name, { term: sent, page: null });
  }
  const urls = svc.requestUrls();

  cases.forEach(([sent, wire], i) => {
    expect(urls[i], `sending ${JSON.stringify(sent)}`).toContain(`p=${wire}`);
    expect(
      new URL(`http://x${urls[i]}`).searchParams.get("p"),
      `${JSON.stringify(sent)} must survive the round trip`,
    ).toBe(sent);
  });

  await svc.stop();
});

// An array repeats the parameter, once per element and in order — `?tag=a&tag=b`, OpenAPI's
// default (form/explode) and what most services read. Before this, an array was refused and
// there was no way to express a repeated parameter at all: `map` is the only builtin, so the
// values could not even be joined into one.
test("query — an array repeats the parameter, in order", async () => {
  const svc = await startMockService(0, { response: { ok: true } });
  const name = `query_arr_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { tags: { type: "array", items: { type: "string" } } },
        required: ["tags"],
      },
      tasks: [{
        id: "call",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${svc.port}/s`,
          method: "GET",
          query: { tag: "$: input.tags", fixed: "1" },
          responses: { 200: { type: "object" } },
        },
        timeout: 2000,
        switch: [{ goto: "end" }],
      }],
    } as any,
  });
  expect(putErr).toBeUndefined();

  const send = async (tags: string[]) => {
    const { data } = await client.POST("/instances", { body: { process: name, input: { tags } } as any });
    expect(await waitForInstance(data!.id)).toBe("completed");
  };

  await send(["b", "a", "b"]);
  const parsed = new URL(`http://x${svc.requestUrls()[0]}`);
  // Order is the array's, and duplicates survive — neither is a set.
  expect(parsed.searchParams.getAll("tag")).toEqual(["b", "a", "b"]);
  expect(parsed.searchParams.get("fixed")).toBe("1");

  // Elements are escaped individually, so one carrying a separator cannot add a parameter.
  await send(["x&y=z", "p q"]);
  const escaped = new URL(`http://x${svc.requestUrls()[1]}`);
  expect(escaped.searchParams.getAll("tag")).toEqual(["x&y=z", "p q"]);
  expect(svc.requestUrls()[1]).toContain("tag=x%26y%3Dz");

  // An empty array is the same as absent: there is nothing to repeat.
  await send([]);
  const empty = new URL(`http://x${svc.requestUrls()[2]}`);
  expect(empty.searchParams.getAll("tag")).toEqual([]);
  expect(empty.searchParams.get("fixed")).toBe("1");

  await svc.stop();
});

// Two behaviours that only show up on the wire, and that nothing else asserts.
//
// Parameter ORDER is by key, deterministically: Go randomises map iteration, so without the
// sort the same definition and the same input would produce a different url on every attempt —
// which breaks request caches and makes an audit trail impossible to compare against itself.
//
// A null INSIDE an array is skipped rather than sent as the text "null", the same omission the
// scalar case makes one level up.
test("query — parameters are ordered by key, and a null element is skipped", async () => {
  const svc = await startMockService(0, { response: { ok: true } });
  const name = `query_order_${crypto.randomUUID()}`;
  const { error: putErr } = await client.PUT("/definitions", {
    body: {
      name,
      input_schema: {
        type: "object",
        properties: { tags: { type: "array", items: { type: ["string", "null"] } } },
        required: ["tags"],
      },
      tasks: [{
        id: "call",
        action: {
          type: "fetch" as const,
          url: `http://localhost:${svc.port}/s`,
          method: "GET",
          // Declared out of alphabetical order on purpose.
          query: { zebra: "1", alpha: "2", middle: "3", tag: "$: input.tags" },
          responses: { 200: { type: "object" } },
        },
        timeout: 2000,
        switch: [{ goto: "end" }],
      }],
    } as any,
  });
  expect(putErr).toBeUndefined();

  const send = async () => {
    const { data } = await client.POST("/instances", {
      body: { process: name, input: { tags: ["a", null, "b"] } } as any,
    });
    expect(await waitForInstance(data!.id)).toBe("completed");
  };

  await send();
  await send();

  const [first, second] = svc.requestUrls();
  // Byte-identical across attempts — the property the sort exists for.
  expect(second).toBe(first);

  const parsed = new URL(`http://x${first}`);
  expect([...parsed.searchParams.keys()]).toEqual(["alpha", "middle", "tag", "tag", "zebra"]);
  // The null element is omitted, not rendered as "null".
  expect(parsed.searchParams.getAll("tag")).toEqual(["a", "b"]);

  await svc.stop();
});
