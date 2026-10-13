#!/bin/sh
# Generates the credentials the demo runs on, into gitignored files compose mounts as secrets.
# Run once:  ./examples/auth/gen-secrets.sh
#
# seed-tokens holds both credentials; genroc stores their hashes at startup. The ADMIN line is
# for the FIRST start only — store its secret (`genctl config set token …`), then delete the
# line. worker-token stays: the evaluator reads it at every start. specs/api-auth.md §5.3.
set -e
cd "$(dirname "$0")"
GENCTL=${GENCTL:-../../genctl}
[ -x "$GENCTL" ] || { echo "build genctl first: make build" >&2; exit 1; }

if [ -d secrets ]; then
  echo "secrets/ already exists — delete it to mint fresh credentials." >&2
  exit 0
fi

mkdir secrets
WORKER=$("$GENCTL" token generate)
ADMIN=$("$GENCTL" token generate)
printf '%s\n' "$WORKER" > secrets/worker-token
printf 'evaluator=worker=%s\nadmin=admin=%s\n' "$WORKER" "$ADMIN" > secrets/seed-tokens
# 0644 so the containers' non-root users can read the bind mount: a development default.
chmod 644 secrets/worker-token secrets/seed-tokens
# The secrets are NOT echoed: printing them only adds a copy to terminal scrollback.
cat <<'NEXT'
wrote examples/auth/secrets/

  seed-tokens   both credentials. Copy the admin secret out (genctl config set token <value>),
                then delete the admin line after the first start: genroc keeps its hash.
  worker-token  stays — the evaluator reads it at every start.
NEXT
