import { defineConfig } from "vitest/config";

const pgProject = process.env.POSTGRES_DSN
  ? [
      {
        test: {
          name: "postgres",
          globalSetup: ["./helpers/server-pg.ts"],
          include: ["integration/**/*_test.ts", "cli/**/*_test.ts"],
          testTimeout: 30_000,
          env: {
            GENROC_PORT: "8889",
            POSTGRES_DSN: process.env.POSTGRES_DSN,
          },
        },
      },
    ]
  : [];

export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: "sqlite",
          globalSetup: ["./helpers/server.ts"],
          include: ["integration/**/*_test.ts", "cli/**/*_test.ts", "tick/**/*_test.ts"],
          testTimeout: 60_000,
          env: { GENROC_PORT: "8888" },
        },
      },
      ...pgProject,
      {
        // No globalSetup: the language server answers from the definition language alone.
        test: {
          name: "lsp",
          include: ["lsp/**/*_test.ts"],
          testTimeout: 60_000,
        },
      },
      {
        // No globalSetup, and its own vitest invocation (package.json `test`): the postgres project's
        // server is a full worker on the same database and would claim the stress suites' instances.
        test: {
          name: "stress",
          include: ["stress/**/*_test.ts"],
          testTimeout: 120_000,
          // One file at a time: each saturates a worker fleet against the single Postgres.
          fileParallelism: false,
          env: process.env.POSTGRES_DSN
            ? { POSTGRES_DSN: process.env.POSTGRES_DSN }
            : {},
        },
      },
    ],
  },
});
