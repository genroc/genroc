import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider, useMutation, useQuery } from "@tanstack/react-query";
import "./style.css";

// The login page, its own bundle: it must render before any session exists, so it shares no code
// path with the app behind the session (specs/ui-issued-tokens.md §5). It talks to genroc-ui's
// /auth, which is outside genroc's spec, so these calls are plain fetches.

type Options = { providers: { id: string; name: string }[]; passwords: boolean };

/** Where to land afterwards. genroc-ui sanitises this again on its side; the check here only
 *  stops the page building a link it should not. */
function returnTo(): string {
  const rd = new URLSearchParams(location.search).get("rd") ?? "/";
  return rd.startsWith("/") && !rd.startsWith("//") && !rd.startsWith("/auth/") ? rd : "/";
}

const unreachable = "Could not reach the server.";

function Login() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const rd = returnTo();

  // Which ways in exist is not a secret -- the buttons announce it -- so this is served without
  // a credential, which it has to be: nothing here has one yet.
  const options = useQuery({
    queryKey: ["auth", "options"],
    queryFn: async () => {
      const r = await fetch("/auth/options");
      if (!r.ok) throw new Error(unreachable);
      return (await r.json()) as Options;
    },
    staleTime: Infinity,
    retry: false, // the page has nothing to show until this answers, so say so at once
  });

  const signIn = useMutation({
    mutationFn: async () => {
      const res = await fetch("/auth/password", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email, password }),
      }).catch(() => {
        throw new Error(unreachable);
      });
      if (res.ok) return;
      const body = await res.json().catch(() => null);
      throw new Error(body?.error ?? "That email and password did not match.");
    },
    // The session cookie arrived on this response; a full navigation is what picks it up for the
    // app, and it lands where the login was started from.
    onSuccess: () => location.assign(rd),
  });

  const error = options.error ? unreachable : signIn.error?.message;
  const opts = options.data;

  if (!opts) return <main className="login"><h1>genroc</h1>{error && <p className="err">{error}</p>}</main>;

  return (
    <main className="login">
      <h1>genroc</h1>
      {error && <p className="err">{error}</p>}

      {opts.providers.map((p) => (
        <a
          key={p.id}
          className="btn"
          href={`/auth/login?provider=${encodeURIComponent(p.id)}&rd=${encodeURIComponent(rd)}`}
        >
          {p.name || p.id}
        </a>
      ))}

      {opts.providers.length > 0 && opts.passwords && <div className="sep">or</div>}

      {opts.passwords && (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            signIn.mutate();
          }}
        >
          <input
            type="email" placeholder="email" autoComplete="username" required autoFocus
            value={email} onChange={(e) => setEmail(e.target.value)}
          />
          <input
            type="password" placeholder="password" autoComplete="current-password" required
            value={password} onChange={(e) => setPassword(e.target.value)}
          />
          <button type="submit" disabled={signIn.isPending}>{signIn.isPending ? "Signing in…" : "Sign in"}</button>
        </form>
      )}

      {opts.providers.length === 0 && !opts.passwords && (
        <p className="err">No login is configured on this server.</p>
      )}
    </main>
  );
}

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={new QueryClient()}>
      <Login />
    </QueryClientProvider>
  </StrictMode>,
);
