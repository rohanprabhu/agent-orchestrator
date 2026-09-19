#!/usr/bin/env bash
set -euo pipefail

# Local integration service only. Agent sessions still run in the desktop daemon.
cloud_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v podman >/dev/null || { echo "Install Podman and start its machine first." >&2; exit 1; }
podman info >/dev/null
command -v go >/dev/null

state_dir="${AO_DATA_DIR:-$HOME/.ao}/lenticular-linear"
container_name=lenticular-linear-postgres
umask 077
mkdir -p "$state_dir/postgres" "$state_dir/bin"
if [[ ! -s "$state_dir/provider-secret-key" ]]; then
  openssl rand -base64 32 > "$state_dir/provider-secret-key"
fi

if podman container exists "$container_name"; then
  [[ "$(podman inspect --format '{{ index .Config.Labels "app" }}' "$container_name")" == lenticular-linear ]] || {
    echo "Container name is already in use by another application." >&2; exit 1;
  }
  podman start "$container_name" >/dev/null
else
  podman run -d --name "$container_name" --label app=lenticular-linear \
    -p 127.0.0.1:54339:5432 \
    -e POSTGRES_DB=ao_cloud -e POSTGRES_USER=ao_cloud_bootstrap \
    -e POSTGRES_PASSWORD=ao_cloud_local_bootstrap \
    -v "$state_dir/postgres:/var/lib/postgresql/data" \
    -v "$cloud_root/dev/postgres/init.sql:/docker-entrypoint-initdb.d/10-runtime-role.sql:ro" \
    docker.io/library/postgres:17-bookworm >/dev/null
fi
ready=false
for ((attempt=0; attempt<60; attempt++)); do
  if podman exec "$container_name" pg_isready -U ao_cloud_owner -d ao_cloud >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[[ "$ready" == true ]] || { echo "PostgreSQL did not become ready." >&2; exit 1; }

cd "$cloud_root"
go build -o "$state_dir/bin/ao-cloud-migrate" ./cmd/ao-cloud-migrate
go build -o "$state_dir/bin/ao-cloud" ./cmd/ao-cloud
export AO_CLOUD_MIGRATION_DATABASE_URL='postgres://ao_cloud_owner:ao_cloud_local_owner@127.0.0.1:54339/ao_cloud?sslmode=disable'
export AO_CLOUD_RUNTIME_DATABASE_USER=ao_cloud_app
"$state_dir/bin/ao-cloud-migrate"

export AO_CLOUD_ENV=development
export AO_CLOUD_HTTP_ADDRESS=127.0.0.1:8080
export AO_CLOUD_DATABASE_URL='postgres://ao_cloud_app:ao_cloud_local_app@127.0.0.1:54339/ao_cloud?sslmode=disable'
export AO_CLOUD_MIGRATE_ON_STARTUP=false
export AO_CLOUD_LOCAL_AUTH=true
# The legacy ecs selection has no sandbox reconciler or provisioning client.
# This stack does not configure any remote execution provider.
export AO_CLOUD_SANDBOX_PROVIDER=ecs
export AO_CLOUD_REPOSITORY_BROKER_URL=
export AO_CLOUD_REPOSITORY_BROKER_TOKEN=
export AO_CLOUD_ENV_CONTROL_TOKEN=
export AO_CLOUD_WORKOS_ISSUER=
export AO_CLOUD_WORKOS_CLIENT_ID=
export AO_CLOUD_WORKOS_API_KEY=
export AO_CLOUD_PROVIDER_SECRET_KEY="$(<"$state_dir/provider-secret-key")"

cat <<'EOF'
Lenticular integration service: http://127.0.0.1:8080
Launch the desktop from frontend/ with:
  AO_CLOUD_OFFERING=off AO_CLOUD_CONTROL_PLANE_URL=http://127.0.0.1:8080 npm run dev
In Settings → Integrations → Linear, sign in and register a local account.
Live Linear OAuth requires AO_CLOUD_LINEAR_* configuration before starting.
Ctrl-C stops the service. Stop PostgreSQL with: podman stop lenticular-linear-postgres
Local data is retained for the next run.
EOF
exec "$state_dir/bin/ao-cloud"
