import { useCallback, useEffect, useState } from "react";
import { ApiError, actor, getInstance, listInstances, setToken, signOut, token, type Instance } from "./api.ts";
import { Tokens } from "./Tokens.tsx";

// Deliberately small: a list of instances and one detail view. genroc's own answer to "what is
// happening" is `genctl instances`, and this is the same question with a mouse — enough to be
// useful, not so much that it grows a second opinion about the domain.

const REFRESH_MS = 3000;

export function App() {
  const [tok, setTok] = useState(token());
  const [status, setStatus] = useState("");
  const [rows, setRows] = useState<Instance[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | Error | null>(null);
  const [loading, setLoading] = useState(false);
  const [view, setView] = useState<"instances" | "tokens">("instances");
  // Reported by the server on every response, never inferred: "it worked and I sent nothing"
  // means `-auth none` OR a proxy that authenticated us, and those are opposite things.
  const [who, setWho] = useState<string | null>(null);

  // No credential of its own behind a proxy: the proxy attaches the JWT. Requests simply go
  // out, and a 401 means there is no proxy and nothing was pasted — which the header renders as
  // the input rather than as an error.

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const q = new URLSearchParams();
      if (status) q.set("status", status);
      const page = await listInstances(q.toString());
      setRows(page.items ?? []);
      setError(null);
      setWho(actor());
    } catch (e) {
      setWho(actor());
      setError(e as Error);
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [status]);

  // Polling rather than a stream: genroc has no change feed, and a 3s poll of one page is
  // cheaper to reason about than a reconnecting socket for a screen someone watches for a
  // minute. It pauses while a detail is open, which is where the reader's attention is.
  useEffect(() => {
    if (view !== "instances") return;
    void load();
    if (selected) return;
    const t = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(t);
  }, [load, selected, view]);

  function saveToken(v: string) {
    setToken(v);
    setTok(v);
  }

  return (
    <main>
      <header>
        <h1>genroc</h1>
        <nav>
          {(["instances", "tokens"] as const).map((v) => (
            <button
              key={v}
              className={view === v ? "tab on" : "tab"}
              onClick={() => setView(v)}
              aria-current={view === v}
            >
              {v}
            </button>
          ))}
        </nav>
        {who?.startsWith("none:") ? (
          <span className="muted" title="genroc was started with -auth none">
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

      {view === "tokens" ? (
        <Tokens />
      ) : (
        <>
          {error && <Problem error={error} />}

          <div className="bar">
            <select value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">all statuses</option>
              {["running", "completed", "failed", "raised", "paused"].map((s) => (
                <option key={s} value={s}>{s}</option>
              ))}
            </select>
            <button onClick={() => void load()} disabled={loading}>
              {loading ? "…" : "refresh"}
            </button>
            <span className="muted">{rows.length} shown</span>
          </div>

          <table>
            <thead>
              <tr><th>id</th><th>process</th><th>status</th><th>task</th><th>updated</th></tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.id} onClick={() => setSelected(r.id)} className="row">
                  <td className="mono">{r.id.slice(0, 8)}</td>
                  <td>{r.process}<span className="muted"> v{r.version}</span></td>
                  <td><Status value={r.status} wait={r.wait_state} /></td>
                  <td className="mono">{r.task ?? "-"}</td>
                  <td className="muted">{new Date(r.updated_at).toLocaleTimeString()}</td>
                </tr>
              ))}
              {!rows.length && !error && (
                <tr><td colSpan={5} className="muted">no instances</td></tr>
              )}
            </tbody>
          </table>
        </>
      )}

      {selected && <Detail id={selected} onClose={() => setSelected(null)} />}
    </main>
  );
}

/** An auth failure is the one error worth explaining rather than printing: the two codes have
 *  different fixes, and the raw message alone sends people to the wrong one. */
function Problem({ error }: { error: Error }) {
  const api = error instanceof ApiError ? error : null;
  return (
    <div className="problem">
      <strong>{api ? `${api.status} ${api.code}` : "error"}</strong> {error.message}
      {api?.status === 401 && <div className="muted">Paste a token above, or mint one in the tokens tab.</div>}
      {api?.status === 403 && <div className="muted">This token lacks the permission that endpoint needs.</div>}
    </div>
  );
}

function Status({ value, wait }: { value: string; wait?: string }) {
  return (
    <span className={`status ${value}`}>
      {value}
      {wait ? <span className="muted"> · {wait}</span> : null}
    </span>
  );
}

function Detail({ id, onClose }: { id: string; onClose: () => void }) {
  const [data, setData] = useState<Awaited<ReturnType<typeof getInstance>> | null>(null);
  const [err, setErr] = useState<Error | null>(null);

  useEffect(() => {
    getInstance(id).then(setData, (e) => setErr(e as Error));
  }, [id]);

  return (
    <div className="overlay" onClick={onClose}>
      <div className="panel" onClick={(e) => e.stopPropagation()}>
        <button className="close" onClick={onClose}>close</button>
        <h2 className="mono">{id}</h2>
        {err && <Problem error={err} />}
        {data && (
          <>
            <dl>
              <dt>process</dt><dd>{data.process} v{data.version}</dd>
              <dt>status</dt><dd><Status value={data.status} wait={data.wait_state} /></dd>
              <dt>task</dt><dd className="mono">{data.task ?? "-"}</dd>
              {data.error_code && (<><dt>error</dt><dd className="mono">{data.error_code}: {data.error_message}</dd></>)}
            </dl>
            <h3>state</h3>
            <pre>{JSON.stringify(data.state ?? {}, null, 2)}</pre>
          </>
        )}
      </div>
    </div>
  );
}
