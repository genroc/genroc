import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "./api.ts";

// Reads and writes go through `$api` (api.ts); this is the cache they share.

/** Polling: genroc has no change feed, and one page every 3s is cheap to reason about. */
export const REFRESH_MS = 3000;

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A 4xx answers the same on a retry, and retrying a 401 only delays the prompt for a token.
      retry: (failures, e) => !(e instanceof ApiError && e.status < 500) && failures < 2,
    },
  },
});
