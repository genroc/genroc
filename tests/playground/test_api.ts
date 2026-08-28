import { createClientTyped } from "../helpers/client";

const client = createClientTyped({ baseUrl: "http://localhost:8888/api" });

async function getAll() {
  let cursor: string | undefined;
  do {
    const response = await client.GET("/instances", {
      params: { query: { order: "asc", after: cursor } },
    });
    response.data?.items?.forEach(({ id, created_at }) =>
      console.log(id, created_at),
    );
    cursor = response.data?.page.after;
  } while (cursor);
}

await getAll();
