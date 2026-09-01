import { useCallback, useEffect, useState } from "react";
import {
  ApiError, getInstance, listInstances, sessionToken, setToken, token, type Instance,
} from "./api.ts";
import { Tokens } from "./Tokens.tsx";

// Deliberately small: a list of instances and one detail view. genroc's own answer to "what is
// happening" is `genctl instances`, and this is the same question with a mouse — enough to be
// useful, not so much that it grows a second opinion about the domain.

const REFRESH_MS = 3000;

export function App() {
  const [tok, setTok] = useState(token());
  const [subject, setSubject] = useState<string | null>(null);
  const [probing, setProbing] = useState(true);
  const [status, setStatus] = useState("");
  const [rows, setRows] = useState<Instance[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [error, setError] = useState<ApiError | Error | null>(null);
  const [loading, setLoading] = useState(false);
  const [view, setView] = useState<"instances" | "tokens">("instances");
  // A request that SUCCEEDS while we hold no credential proves `-auth none`. There is no
  // endpoint that reports the mode, and adding one would publish the deployment's posture to
  // anyone who asks — so this is inferred from the one observation that already answers it.
  const [authOff, setAuthOff] = useState(false);

  // Behind an SSO proxy the browser has a session but no token, so ask the exchange for one
  // before falling back to the paste field. A deployment without a proxy answers 401 or 501
  // here and the field is all there is — which is the current default, not an error.
  //
  // ONLY when we hold none. The exchange cannot return a token it issued before — the server
  // stores just the hash — so every call MINTS one, and asking on each page load left a trail
  // of live credentials behind. A token we already hold is used until something rejects it,
  // which is what `exchange` below is for.
  const exchange = useCallback(async () => {
    const s = await sessionToken();
    if (s?.token) {
      setToken(s.token);
      setTok(s.token);
      setSubject(s.subject);
    }
    return s?.token ?? "";
  }, []);

  useEffect(() => {
    let live = true;
    if (token()) {
      setProbing(false);
      return;
    }
    exchange().finally(() => {
      if (live) setProbing(false);
    });
    return () => {
      live = false;
    };
  }, [exchange]);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const q = new URLSearchParams();
      if (status) q.set("status", status);
      const page = await listInstances(q.toString());
      setRows(page.items ?? []);
      setError(null);
      if (!token()) setAuthOff(true);
    } catch (e) {
      // A session token expires, so a 401 on a token we hold is the ordinary end of a session
      // rather than an error to show. Trade the proxy's identity for a fresh one and retry
      // once; a second failure is real and surfaces.
      if (e instanceof ApiError && e.isAuth && token()) {
        setToken("");
        if (await exchange()) {
          try {
            const q = new URLSearchParams();
            if (status) q.set("status", status);
            const page = await listInstances(q.toString());
            setRows(page.items ?? []);
            setError(null);
            return;
          } catch (retry) {
            setError(retry as Error);
            setRows([]);
            return;
          } finally {
            setLoading(false);
          }
        }
      }
      setError(e as Error);
      setRows([]);
    } finally {
      setLoading(false);
    }
  }, [status, exchange]);

  // Polling rather than a stream: genroc has no change feed, and a 3s poll of one page is
  // cheaper to reason about than a reconnecting socket for a screen someone watches for a
  // minute. It pauses while a detail is open, which is where the reader's attention is.
  useEffect(() => {
    if (probing || view !== "instances") return;
    void load();
    if (selected) return;
    const t = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(t);
  }, [load, selected, probing, view]);

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
        {subject ? (
          <span className="muted" title="identity forwarded by the proxy">signed in as {subject}</span>
        ) : authOff ? (
          <span className="muted" title="genroc was started with -auth none">
            authentication off — every caller is an operator
          </span>
        ) : (
          <input
            type="password"
            placeholder="genroc_sk_… (leave empty if auth is off)"
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
