# syntax=docker/dockerfile:1
#
# genroc/eval-node — the script-task worker. Claims `external` tasks off genroc's queue and
# evaluates each script module in its own realm. Node runs .ts directly, so there is no build.
#
# Named for the runtime because a script task is an ordinary external task: another evaluator
# in another language claims the same queue.
#
# No `npm install`: worker.ts imports only node builtins. rollup and typescript are dependencies
# of import.ts, the AUTHOR-TIME resolver genctl runs locally — 42 MB the worker never loads.
FROM node:24-alpine
WORKDIR /app
COPY eval-node ./eval-node
# GENROC_SERVER is required; GENROC_TOKEN only when the server has auth on.
USER node
ENTRYPOINT ["node", "eval-node/worker.ts"]
