import { ApiError } from "./api.ts";

/** An auth failure is the one error worth explaining rather than printing: the two codes have
 *  different fixes, and the raw message alone sends people to the wrong one. */
export function Problem({ error }: { error: unknown }) {
  const api = error instanceof ApiError ? error : null;
  return (
    <div className="problem">
      <strong>{api ? `${api.status} ${api.code}` : "error"}</strong>{" "}
      {error instanceof Error ? error.message : String(error)}
      {api?.status === 401 && (
        <div className="muted">
          Paste a token above, or mint one in the tokens tab.
        </div>
      )}
      {api?.status === 403 && (
        <div className="muted">
          This token lacks the permission that endpoint needs.
        </div>
      )}
    </div>
  );
}

export function Status({ value, phase }: { value: string; phase?: string }) {
  return (
    <span className={`status ${value}`}>
      {value}
      {phase ? <span className="muted"> · {phase}</span> : null}
    </span>
  );
}
