import { beforeAll, expect, test } from "vitest";
import { client } from "../helpers/client.ts";

// A unique process so this file's instances are identifiable in the global list
// (the /instances endpoint filters by status, not process). The list is shared
// and (on Postgres) large and concurrently written, so tests bound how far they
// page rather than walking the whole table.
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
          timeout_ms: 200,
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

// walk pages forward following page.after until it is absent: the final
// page, until maxPages, or — when `until` is given — once every id in it has been
// seen.
async function walk(
  query: Query,
  opts: { maxPages?: number; until?: Set<string> } = {},
): Promise<{ items: Item[]; pages: number }> {
  const { maxPages = 15, until } = opts;
  const remaining = until ? new Set(until) : undefined;
  const items: Item[] = [];
  let after: string | undefined;
  let pages = 0;
  while (pages < maxPages) {
    const { data, error } = await client.GET("/instances", {
      params: { query: { ...query, after } },
    });
    expect(error).toBeUndefined();
    pages++;
    for (const it of data!.items ?? []) {
      items.push(it as Item);
      remaining?.delete(it.id);
    }
    if (remaining && remaining.size === 0) break;
    if (!data!.page.after) break; // absent once no rows remain after
    after = data!.page.after;
  }
  return { items, pages };
}

test("page object echoes the effective sort and order", async () => {
  const { data, error } = await client.GET("/instances", { params: { query: { limit: 2 } } });
  expect(error).toBeUndefined();
  const page = data!.page;
  expect(page.size).toBe(2);
  // The effective sort/order is echoed back (resolved from the endpoint defaults).
  expect(page.sort).toBe("created");
  expect(page.order).toBe("desc");
  expect(page.after).toBeTruthy(); // far more than 2 rows exist
  expect((data!.items ?? []).length).toBeLessThanOrEqual(2);
  // items_before / page.before are asserted below on an ascending page instead:
  // on this newest-first page they are racy — see the next test.
});

test("page object reports position", async () => {
  // Oldest-first on purpose. The counts are two bounded subqueries run after the
  // page itself, and /instances is a shared, concurrently written table: on a
  // newest-first page an instance another test creates in between sorts *before*
  // the page, so items_before flakes to 1. Ascending, a concurrent insert can only
  // land after the page, so a first page has 0 rows before it by construction.
  const { data, error } = await client.GET("/instances", {
    params: { query: { limit: 2, order: "asc" } },
  });
  expect(error).toBeUndefined();
  const page = data!.page;
  expect(page.order).toBe("asc");
  expect(page.items_before).toBe(0); // first page
  expect(page.items_after).toBeGreaterThan(0); // far more than 2 rows exist
  // Cursor present only in a direction with more rows: first page → after only.
  expect(page.after).toBeTruthy();
  expect(page.before).toBeFalsy();
});

test("forward paging has no duplicates and is newest-first", async () => {
  // limit 2 over our own N=5 instances spans >1 page even when this file runs alone.
  const { items, pages } = await walk({ limit: 2 }, { maxPages: 12 });
  expect(pages).toBeGreaterThan(1);
  expect(new Set(items.map((i) => i.id)).size).toBe(items.length); // no dupes
  for (let i = 1; i < items.length; i++) {
    expect(items[i - 1].created_at >= items[i].created_at).toBe(true); // created desc
  }
});

test("paging forward then backward returns the original page", async () => {
  const p1 = await client.GET("/instances", { params: { query: { limit: 2 } } });
  expect(p1.error).toBeUndefined();
  const firstIds = (p1.data!.items ?? []).map((i) => i.id);

  const p2 = await client.GET("/instances", {
    params: { query: { limit: 2, after: p1.data!.page.after } },
  });
  expect(p2.error).toBeUndefined();
  expect(p2.data!.page.before).toBeTruthy();

  // Step back from page 2 → exactly page 1, in the same order.
  const back = await client.GET("/instances", {
    params: { query: { limit: 2, before: p2.data!.page.before } },
  });
  expect(back.error).toBeUndefined();
  expect((back.data!.items ?? []).map((i) => i.id)).toEqual(firstIds);
});

test("order=asc lists oldest-first", async () => {
  const { items } = await walk({ order: "asc", limit: 5 }, { maxPages: 12 });
  for (let i = 1; i < items.length; i++) {
    expect(items[i - 1].created_at <= items[i].created_at).toBe(true);
  }
});

test("returns every newly created instance in newest-first order", async () => {
  const { items } = await walk({ limit: 50 }, { until: new Set(ids), maxPages: 40 });
  const mine = items.filter((i) => i.process === processName).map((i) => i.id);
  expect(mine).toEqual([...ids].reverse());
});

test("a cursor is rejected when reused under a different direction", async () => {
  // Minted under the default (created desc); replaying it with order=asc must be
  // rejected — the cursor carries the sort+direction it was issued for.
  const first = await client.GET("/instances", { params: { query: { limit: 1 } } });
  expect(first.error).toBeUndefined();
  const after = first.data!.page.after;
  expect(after).toBeTruthy();

  const reused = await client.GET("/instances", {
    params: { query: { order: "asc", after } },
  });
  expect(reused.error).toBeDefined();
  expect(reused.data).toBeUndefined();
});

test("an unknown sort key is rejected", async () => {
  const { data, error } = await client.GET("/instances", { params: { query: { sort: "bogus" } } });
  expect(error).toBeDefined();
  expect(data).toBeUndefined();
});
