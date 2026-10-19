import { keepPreviousData } from "@tanstack/react-query";
import { Outlet, createFileRoute, useMatchRoute, useNavigate } from "@tanstack/react-router";
import { $api, STATUSES, type Status as StatusName } from "../api.ts";
import { Problem, Status } from "../components.tsx";
import { REFRESH_MS } from "../queries.ts";

// Deliberately small: instances and one detail view, `genctl instances` with a mouse. It must not
// grow a second opinion about the domain.

export const Route = createFileRoute("/instances")({
  // Explicitly undefined, never omitted: the root passes the raw query through, and an omitted key
  // would leave an unknown status in place to be sent.
  validateSearch: (s: Record<string, unknown>): { status?: StatusName } => ({
    status: STATUSES.includes(s.status as StatusName) ? (s.status as StatusName) : undefined,
  }),
  component: InstancesPage,
});

function InstancesPage() {
  const { status } = Route.useSearch();
  const navigate = useNavigate({ from: "/instances" });
  // Paused while a detail is open: the list behind it is not being read.
  const detailOpen = !!useMatchRoute()({ to: "/instances/$id" });
  const list = $api.useQuery(
    "get",
    "/instances",
    { params: { query: { status } } },
    {
      refetchInterval: detailOpen ? false : REFRESH_MS,
      placeholderData: keepPreviousData,
    },
  );
  const rows = list.error ? [] : (list.data?.items ?? []);

  return (
    <>
      {list.error && <Problem error={list.error} />}

      <div className="bar">
        <select
          value={status ?? ""}
          onChange={(e) =>
            void navigate({
              search: (s) => ({
                ...s,
                status: (e.target.value || undefined) as StatusName | undefined,
              }),
            })
          }
        >
          <option value="">all statuses</option>
          {STATUSES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <button onClick={() => void list.refetch()} disabled={list.isFetching}>
          {list.isFetching ? "…" : "refresh"}
        </button>
        <span className="muted">{rows.length} shown</span>
      </div>

      <table>
        <thead>
          <tr>
            <th>id</th>
            <th>process</th>
            <th>status</th>
            <th>task</th>
            <th>updated</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr
              key={r.id}
              onClick={() =>
                void navigate({
                  to: "/instances/$id",
                  params: { id: r.id },
                  search: true,
                })
              }
              className="row"
            >
              <td className="mono">{r.id.slice(0, 8)}</td>
              <td>
                {r.process}
                <span className="muted"> v{r.version}</span>
              </td>
              <td>
                <Status value={r.status} phase={r.phase} />
              </td>
              <td className="mono">{r.task ?? "-"}</td>
              <td className="muted">
                {new Date(r.updated_at).toLocaleTimeString()}
              </td>
            </tr>
          ))}
          {!rows.length && !list.error && !list.isPending && (
            <tr>
              <td colSpan={5} className="muted">
                no instances
              </td>
            </tr>
          )}
        </tbody>
      </table>

      <Outlet />
    </>
  );
}
