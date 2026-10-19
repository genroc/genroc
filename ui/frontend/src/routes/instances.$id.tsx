import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { stringify } from "yaml";
import { $api } from "../api.ts";
import { Problem, Status } from "../components.tsx";

// A child of /instances, so the list stays drawn behind the panel and keeps its filter.
export const Route = createFileRoute("/instances/$id")({ component: Detail });

function Detail() {
  const { id } = Route.useParams();
  const navigate = useNavigate({ from: "/instances/$id" });
  const { data, error } = $api.useQuery("get", "/instances/{id}/detail", {
    params: { path: { id } },
  });
  const close = () => void navigate({ to: "/instances", search: true });

  return (
    <div className="overlay" onClick={close}>
      <div className="panel" onClick={(e) => e.stopPropagation()}>
        <button className="close" onClick={close}>
          close
        </button>
        <h2 className="mono">{id}</h2>
        {error && <Problem error={error} />}
        {data && (
          <>
            <dl>
              <dt>process</dt>
              <dd>
                {data.process} v{data.version}
              </dd>
              <dt>status</dt>
              <dd>
                <Status value={data.status} phase={data.phase} />
              </dd>
              <dt>task</dt>
              <dd className="mono">{data.task ?? "-"}</dd>
              {data.error_code && (
                <>
                  <dt>error</dt>
                  <dd className="mono">
                    {data.error_code}: {data.error_message}
                  </dd>
                </>
              )}
            </dl>
            {data.output !== undefined && (
              <>
                <h3>output</h3>
                <pre>{stringify(data.output)}</pre>
              </>
            )}
            {data.error_data !== undefined && (
              <>
                <h3>error data</h3>
                <pre>{stringify(data.error_data)}</pre>
              </>
            )}
            {data.external_input !== undefined && (
              <>
                <h3>external input</h3>
                <pre>{stringify(data.external_input)}</pre>
              </>
            )}
            <h3>state</h3>
            <pre>{stringify(data.state ?? {})}</pre>
          </>
        )}
      </div>
    </div>
  );
}
