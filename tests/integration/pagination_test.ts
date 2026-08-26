import { beforeAll, expect, test } from "vitest";
import { client } from "../helpers/client.ts";

// Every query here is scoped to this file's own process. /instances IS filterable by
// process, and without that filter these tests page a table other files are concurrently
// writing: a row inserted between two calls shifts a newest-first page, which made
// "paging forward then backward" flake on Postgres and forced the rest of the file into
// workarounds (assert ascending only, bound maxPages, filter the walk afterwards). Scoped,
// the fixture is exactly N rows nobody else touches, so each property can be asserted
// outright.
const processName = `paginate_proc_${crypto.randomUUID()}`;
const N = 5;
const ids: string[] = [];

beforeAll(async () => {
  await client.PUT("/definitions", {
    body: {
      name: processName,
      tasks: [
        {
          id: "s1",
          action: { type: "fetch" as const, url: "http://localhost:19991/action" },
          timeout: 200,
          switch: [{ goto: "end" }],
        },
      ],
    },
  });
  // Start N instances in sequence so their UUIDv7 ids are strictly time-ordered.
  for (let i = 0; i < N; i++) {
    const { data, error } = await client.POST("/instances", { body: { process: processName } });
    expect(error).toBeUndefined();
    ids.push(data!.id);
  }
});

type Item = { id: string; process: string; created_at: string };
type Query = { sort?: string; order?: "asc" | "desc"; limit?: number };

// walk pages forward through this process's rows until page.after is absent. The bound is
// a runaway guard, not a workaround: scoped to N rows the walk terminates on its own, and
// a walk that did not would otherwise hang the suite rather than fail it.
const maxPages = 15;

async function walk(query: Query): Promise<{ items: Item[]; pages: number }> {
  const items: Item[] = [];
  let after: string | undefined;
  let pages = 0;
  while (pages < maxPages) {
    const { data, error } = await client.GET("/instances", {
      params: { query: { ...query, process: processName, after } },
    });
    expect(error).toBeUndefined();
    pages++;
    items.push(...((data!.items ?? []) as Item[]));
    if (!data!.page.after) break; // absent once no rows remain after
    after = data!.page.after;
  }
  expect(pages, "the walk hit its runaway guard").toBeLessThan(maxPages);
  return { items, pages };
}

test("page object echoes the effective sort and order", async () => {
  const { data, error } = await client.GET("/instances", {
    params: { query: { process: processName, limit: 2 } },
  });
  expect(error).toBeUndefined();
  const page = data!.page;
  expect(page.size).toBe(2);
  // The effective sort/order is echoed back (resolved from the endpoint defaults).
  expect(page.sort).toBe("created");
  expect(page.order).toBe("desc");
  expect(page.after).toBeTruthy(); // N > 2 rows in this process
  expect((data!.items ?? []).length).toBe(2);
});

test("page object reports position", async () => {
  // Asserted in BOTH directions. This used to be ascending-only, because on a shared table
  // an instance another file created between the page and its count sorts before a
  // newest-first page and flaked items_before to 1. Scoped to N fixed rows, the counts are
  // exact either way.
  for (const order of ["asc", "desc"] as const) {
    const { data, error } = await client.GET("/instances", {
      params: { query: { process: processName, limit: 2, order } },
    });
    expect(error).toBeUndefined();
    const page = data!.page;
    expect(page.order).toBe(order);
    expect(page.items_before, `${order}: first page`).toBe(0);
    expect(page.items_after, `${order}: N=${N} minus the page`).toBe(N - 2);
    // Cursor present only in a direction with more rows: first page → after only.
    expect(page.after).toBeTruthy();
    expect(page.before).toBeFalsy();
  }
});

test("forward paging has no duplicates and is newest-first", async () => {
  const { items, pages } = await walk({ limit: 2 });
  expect(pages).toBeGreaterThan(1); // N=5 over limit 2
  expect(items.map((i) => i.id)).toEqual([...ids].reverse()); // every row, exactly once
  for (let i = 1; i < items.length; i++) {
    expect(items[i - 1].created_at >= items[i].created_at).toBe(true); // created desc
  }
});

test("paging forward then backward returns the original page", async () => {
  // Unscoped, this was the file's one genuine flake: three calls against a table other
  // files write to, where a row inserted after p1 shifts what "the newest 2" means, so the
  // step back returns a page that never equalled p1.
  const page = (q: Record<string, unknown>) =>
    client.GET("/instances", { params: { query: { process: processName, limit: 2, ...q } } });

  const p1 = await page({});
  expect(p1.error).toBeUndefined();
  const firstIds = (p1.data!.items ?? []).map((i) => i.id);
  expect(firstIds).toEqual([...ids].reverse().slice(0, 2));

  const p2 = await page({ after: p1.data!.page.after });
  expect(p2.error).toBeUndefined();
  expect(p2.data!.page.before).toBeTruthy();

  // Step back from page 2 → exactly page 1, in the same order.
  const back = await page({ before: p2.data!.page.before });
  expect(back.error).toBeUndefined();
  expect((back.data!.items ?? []).map((i) => i.id)).toEqual(firstIds);
});

test("order=asc lists oldest-first", async () => {
  const { items } = await walk({ order: "asc", limit: 2 });
  expect(items.map((i) => i.id)).toEqual(ids);
  for (let i = 1; i < items.length; i++) {
    expect(items[i - 1].created_at <= items[i].created_at).toBe(true);
  }
});

test("returns every newly created instance in newest-first order", async () => {
  // One page big enough for the fixture: no walking, no post-filter, no page bound.
  const { items, pages } = await walk({ limit: 50 });
  expect(pages).toBe(1);
  expect(items.map((i) => i.id)).toEqual([...ids].reverse());
});

test("a cursor is rejected when reused under a different direction", async () => {
  // Minted under the default (created desc); replaying it with order=asc must be
  // rejected — the cursor carries the sort+direction it was issued for.
  const first = await client.GET("/instances", {
    params: { query: { process: processName, limit: 1 } },
  });
  expect(first.error).toBeUndefined();
  const after = first.data!.page.after;
  expect(after).toBeTruthy();

  const reused = await client.GET("/instances", {
    params: { query: { process: processName, order: "asc", after } },
  });
  expect(reused.error).toBeDefined();
  expect(reused.data).toBeUndefined();
});

test("an unknown sort key is rejected", async () => {
  const { data, error } = await client.GET("/instances", { params: { query: { sort: "bogus" } } });
  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
