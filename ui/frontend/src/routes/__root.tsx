import { useState, useSyncExternalStore } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, createRootRoute } from "@tanstack/react-router";
import { actor, onActorChange, setToken, signOut, token } from "../api.ts";

// Routes are files under src/routes, and real paths, so a detail can be linked: genroc-ui answers
// any path that is not a file or /api with this app, behind the login (specs/ui-component.md).

export const Route = createRootRoute({ component: Layout, notFoundComponent: NotFound });

function Layout() {
  const queryClient = useQueryClient();
  const [tok, setTok] = useState(token());
  // Reported by the server on every response, never inferred: "it worked and I sent nothing"
  // means no `--auth` OR a proxy that authenticated us, and those are opposite things.
  const who = useSyncExternalStore(onActorChange, actor);

  function saveToken(v: string) {
    setToken(v);
    setTok(v);
    void queryClient.invalidateQueries(); // every answer so far was given to the old credential
  }

  return (
    <main>
      <header>
        <h1>genroc</h1>
        <nav>
          {(["/instances", "/tokens"] as const).map((to) => (
            <Link key={to} to={to} className="tab" activeProps={{ className: "on" }}>
              {to.slice(1)}
            </Link>
          ))}
        </nav>
        {who?.startsWith("no-auth:") ? (
          <span className="muted" title="genroc was started without --auth">
            authentication off — every caller is an operator
          </span>
        ) : who ? (
          <span className="muted" title={`genroc attributes your writes to ${who}`}>
            signed in as {who.slice(who.indexOf(":") + 1)}
            {who.startsWith("jwt:") && (
              // Only for a session this app can end. A pasted token is not a session, and
              // signing out of one would clear a credential the person typed in.
              <>
                {" "}
                <button className="link" onClick={() => void signOut()} title="Sign out. Also how you pick up a change to your own groups.">
                  sign out
                </button>
              </>
            )}
          </span>
        ) : (
          <input
            type="password"
            placeholder="genroc_sk_… or a JWT (empty behind a proxy, or if auth is off)"
            value={tok}
            onChange={(e) => saveToken(e.target.value)}
            spellCheck={false}
          />
        )}
      </header>
      <Outlet />
    </main>
  );
}

function NotFound() {
  return (
    <p className="muted">
      No such page. <Link to="/instances">instances</Link>
    </p>
  );
}
