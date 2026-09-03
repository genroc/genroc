import { resolve } from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server PROXIES /api to genroc, which is the whole reason there is no CORS code in
// this project: the browser only ever talks to one origin (the Vite server), and the hop to
// genroc is server-to-server. In production the same shape holds for a different reason — the
// UI is served by genroc itself at `/`, so /api is same-origin already. specs/api-auth.md §5.1.
export default defineConfig({
  plugins: [react()],
  // Two entry points, two bundles. The login page must render before any session exists, so it
  // cannot be a route inside the app it is gating -- and as its own bundle it does not ship the
  // app's code to draw a password field. specs/ui-issued-tokens.md §5.
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
        login: resolve(__dirname, "login.html"),
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: process.env.GENROC_SERVER ?? "http://localhost:8448", changeOrigin: true },
      "/healthz": { target: process.env.GENROC_SERVER ?? "http://localhost:8448", changeOrigin: true },
      "/auth": { target: process.env.GENROC_SERVER ?? "http://localhost:8448", changeOrigin: true },
    },
  },
});
