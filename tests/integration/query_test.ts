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
            method: "get",
            query,
            responses: { 200: { type: "object", properties: { ok: { type: "boolean" } } } },
            timeout: 2000,
          },
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

// Interpolating into the url escapes nothing, and a term can come from untrusted process input.
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

// Space is `%20`, not url.Values' `+`: under RFC 3986 `+` is a literal plus, and a server reading
// it that way takes the wrong value silently. `%20` is a space under both readings.
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

// `?tag=a&tag=b`: OpenAPI's default (form/explode).
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
          method: "get",
          query: { tag: "$: input.tags", fixed: "1" },
          responses: { 200: { type: "object" } },
          timeout: 2000,
        },
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

// Sorted because Go randomises map iteration: unsorted, the same input gives a different url per
// attempt, breaking request caches and audit comparison.
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
          method: "get",
          // Declared out of alphabetical order on purpose.
          query: { zebra: "1", alpha: "2", middle: "3", tag: "$: input.tags" },
          responses: { 200: { type: "object" } },
          timeout: 2000,
        },
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
