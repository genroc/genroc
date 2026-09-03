import { StrictMode, useEffect, useState } from "react";
import { createRoot } from "react-dom/client";
import "./style.css";

// The login page, built as its own bundle. It is the one screen that must render before any
// session exists, so it shares no code path with the app behind the session -- and being a
// separate entry point means it does not carry the app's weight to do it.
// specs/ui-issued-tokens.md §5.

type Options = { providers: { id: string; name: string }[]; passwords: boolean };

/** Where to land afterwards. genroc-ui sanitises this again on its side; the check here only
 *  stops the page building a link it should not. */
function returnTo(): string {
  const rd = new URLSearchParams(location.search).get("rd") ?? "/";
  return rd.startsWith("/") && !rd.startsWith("//") && !rd.startsWith("/auth/") ? rd : "/";
}

function Login() {
  const [opts, setOpts] = useState<Options | null>(null);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const rd = returnTo();

  // Which ways in exist is not a secret -- the buttons announce it -- so this is served without
  // a credential, which it has to be: nothing here has one yet.
  useEffect(() => {
    fetch("/auth/options")
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`))))
      .then(setOpts)
      .catch(() => setError("Could not reach the server."));
  }, []);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/auth/password", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      if (res.ok) {
        // The session cookie arrived on this response; a full navigation is what picks it up
        // for the app, and it lands where the login was started from.
        location.href = rd;
        return;
      }
      const body = await res.json().catch(() => null);
      setError(body?.error ?? "That email and password did not match.");
    } catch {
      setError("Could not reach the server.");
    } finally {
      setBusy(false);
    }
  }

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
        <form onSubmit={submit}>
          <input
            type="email" placeholder="email" autoComplete="username" required autoFocus
            value={email} onChange={(e) => setEmail(e.target.value)}
          />
          <input
            type="password" placeholder="password" autoComplete="current-password" required
            value={password} onChange={(e) => setPassword(e.target.value)}
          />
          <button type="submit" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
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
    <Login />
  </StrictMode>,
);
