#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ -f .env ]; then
  while IFS='=' read -r name value; do
    case "$name" in FLAGFORGE_ADMIN_TOKEN|FLAGFORGE_API_PORT|FLAGFORGE_EXAMPLE_PORT) if [ -z "${!name:-}" ]; then export "$name=$value"; fi ;; esac
  done < .env
fi
command -v jq >/dev/null
export FLAGFORGE_ADMIN_TOKEN="${FLAGFORGE_ADMIN_TOKEN:-api_admin_$(openssl rand -hex 24)}"
docker compose up -d --build postgres redis server otel
api="http://localhost:${FLAGFORGE_API_PORT:-18080}"
example="http://localhost:${FLAGFORGE_EXAMPLE_PORT:-18081}"
for _ in $(seq 1 90); do curl -fsS "$api/readyz" >/dev/null && break || sleep 1; done
curl -fsS "$api/readyz" >/dev/null
admin() { curl -fsS -H "Authorization: Bearer $FLAGFORGE_ADMIN_TOKEN" -H 'Content-Type: application/json' "$@"; }
project="demo-$(date +%s)"
admin -X POST "$api/api/v1/projects" -d "{\"key\":\"$project\",\"name\":\"Demo\"}" >/dev/null
export FLAGFORGE_SDK_KEY
FLAGFORGE_SDK_KEY=$(admin -X POST "$api/api/v1/projects/$project/environments" -d '{"key":"production","name":"Production"}' | jq -r .sdk_key)
admin -X POST "$api/api/v1/projects/$project/flags" -d '{"key":"new-checkout","name":"New checkout","type":"BOOLEAN"}' >/dev/null
path="$api/api/v1/projects/$project/environments/production/flags/new-checkout"
admin -X PUT "$path" -H 'If-Match: 0' -d '{"type":"BOOLEAN","enabled":false,"variations":{"on":true,"off":false},"default_variation":"off","off_variation":"off"}' >/dev/null
docker compose --profile demo up -d --build example
for _ in $(seq 1 30); do curl -fsS "$example/healthz" >/dev/null && break || sleep 1; done
observe() { local expected=$1; for _ in $(seq 1 100); do actual=$(curl -fsS "$example/checkout?user=user-1"); [ "$(jq -r .sdk.version <<<"$actual")" -ge "$expected" ] && return; sleep .1; done; echo 'SDK propagation timeout' >&2; exit 1; }
observe 1
echo 'Initial checkout:'
curl -fsS "$example/checkout?user=user-1" | jq .
for percentage in 10 50; do
  current=$(admin "$path"); version=$(jq -r .version <<<"$current")
  config=$(jq --argjson weight "$((percentage * 1000))" '.enabled=true | .rollout=[{variation_key:"on",weight:$weight},{variation_key:"off",weight:(100000-$weight)}]' <<<"$current")
  version=$(admin -X PUT "$path" -H "If-Match: $version" -d "$config" | jq -r .version)
  observe "$version"
  new=0
  for i in $(seq 1 100); do result=$(curl -fsS "$example/checkout?user=user-$i" | jq -r .checkout); if [ "$result" = new ]; then new=$((new+1)); fi; done
  echo "$percentage% rollout: $new of 100 deterministic users saw new checkout"
done
version=$(admin "$path" | jq -r .version)
version=$(admin -X POST "$path/disable" -H "If-Match: $version" | jq -r .version)
observe "$version"
echo 'Kill switch propagated without restart:'
curl -fsS "$example/checkout?user=user-1" | jq .
echo "Audit: $api/api/v1/audit?project=$project"
