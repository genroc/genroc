import { useCallback, useEffect, useState } from "react";
import { ApiError, PERMS, createToken, listTokens, revokeToken, type ApiToken } from "./api.ts";

// Credential management. specs/api-auth.md §5.
//
// Exists so a proxy-backed deployment needs no seeded credential: an operator logs in as a
// person and mints a machine token here.

const DESC: Record<string, string> = {
  worker: "claim and resolve external tasks — the low-trust inbound zone",
  read: "every GET, plus validate and compat",
  operate: "act on runs: start, pause, resume, retry",
  deploy: "change what runs: definitions, channels, upgrade",
  admin: "everything, including minting and revoking tokens",
};

function status(t: ApiToken): string {
  if (t.revoked_at) return "revoked";
  // Expiry is a status, not just a column: a lapsed token shown as "live" is the row an
  // operator skips while wondering why the caller gets 401.
  if (t.expires_at && new Date(t.expires_at) <= new Date()) return "expired";
  return "live";
}

const when = (s?: string) => (s ? new Date(s).toLocaleString() : "—");

export function Tokens() {
  const [rows, setRows] = useState<ApiToken[]>([]);
  const [error, setError] = useState<Error | null>(null);
  const [label, setLabel] = useState("");
  const [perms, setPerms] = useState<string[]>(["read"]);
  const [minted, setMinted] = useState<{ token: string; label?: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setRows((await listTokens()).items ?? []);
      setError(null);
    } catch (e) {
      setError(e as Error);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function mint(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    try {
      const t = await createToken(label.trim(), perms);
      // Held in component state only, never in localStorage: this is the one moment the
      // plaintext exists, and persisting it would make a copy nothing later can revoke.
      setMinted({ token: t.token, label: t.label });
      setCopied(false);
      setLabel("");
      await load();
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
    }
  }

  async function revoke(t: ApiToken) {
    if (!confirm(`Revoke ${t.label || t.id}? Callers presenting it are refused from the next request.`)) return;
    try {
      await revokeToken(t.id);
      await load();
    } catch (e) {
      setError(e as Error);
    }
  }

  const denied = error instanceof ApiError && error.status === 403;
  if (denied) {
    return (
      <p className="problem">
        Minting tokens needs the <code>admin</code> permission, and this session does not have it.
        An operator with admin can create one for you, or <code>genroc token create</code> against
        the database can.
      </p>
    );
  }

  return (
    <>
      {error && !denied && <p className="problem">{error.message}</p>}

      {minted && (
        <div className="minted">
          <div className="bar">
            <strong>Copy this now.</strong>
            <span className="muted">
              Only the hash is stored, so it cannot be shown again — a lost token is minted again,
              not recovered.
            </span>
          </div>
          <code className="secret">{minted.token}</code>
          <div className="bar">
            <button
              onClick={() => {
                void navigator.clipboard?.writeText(minted.token).then(() => setCopied(true));
              }}
            >
              {copied ? "copied" : "copy"}
            </button>
            <button onClick={() => setMinted(null)}>dismiss</button>
          </div>
        </div>
      )}

      <form className="mint" onSubmit={mint}>
        <div className="bar">
          <input
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            placeholder="label — e.g. laptop, ci, evaluator"
            aria-label="label"
          />
          <button disabled={busy || perms.length === 0}>{busy ? "minting…" : "mint token"}</button>
        </div>
        <ul className="perms">
          {PERMS.map((p) => (
            <li key={p}>
              <label>
                <input
                  type="checkbox"
                  checked={perms.includes(p)}
                  onChange={(e) =>
                    setPerms((cur) => (e.target.checked ? [...cur, p] : cur.filter((x) => x !== p)))
                  }
                />
                <span className="pname">{p}</span>
                <span className="muted">{DESC[p]}</span>
              </label>
            </li>
          ))}
        </ul>
        {perms.length === 0 && <p className="muted">A token with no permissions can do nothing; pick at least one.</p>}
      </form>

      <table>
        <thead>
          <tr>
            <th>Label</th>
            <th>Permissions</th>
            <th>Created</th>
            <th>Last used</th>
            <th>Expires</th>
            <th>Status</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {rows.map((t) => (
            <tr key={t.id}>
              <td>{t.label || <span className="muted">{t.id}</span>}</td>
              <td>{t.perms.join(", ")}</td>
              <td className="mono">{when(t.created_at)}</td>
              <td className="mono">{when(t.last_used_at)}</td>
              <td className="mono">{t.expires_at ? when(t.expires_at) : <span className="muted">never</span>}</td>
              <td className={`status ${status(t)}`}>{status(t)}</td>
              <td>
                {status(t) !== "revoked" && (
                  <button className="link" onClick={() => void revoke(t)}>
                    revoke
                  </button>
                )}
              </td>
            </tr>
          ))}
          {rows.length === 0 && (
            <tr>
              <td colSpan={7} className="muted">
                no tokens
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </>
  );
}
