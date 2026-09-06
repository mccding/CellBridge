#!/usr/bin/env sh
set -eu

project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
compose_file="$project_root/infra/docker/docker-compose.yml"
env_file=${CELLBRIDGE_ENV_FILE:-"$project_root/infra/docker/.env"}
compose_dir=$(CDPATH= cd -- "$(dirname -- "$compose_file")" && pwd)

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker is required; install Docker Engine and Compose v2 first." >&2
    exit 1
fi
if ! command -v tailscale >/dev/null 2>&1; then
    echo "Tailscale is required on the NAS host for Tailnet-only ingress." >&2
    exit 1
fi
if [ ! -f "$env_file" ]; then
    echo "Missing $env_file; copy infra/docker/.env.example and set deployment secrets." >&2
    exit 1
fi

config_file=${CELLBRIDGE_CONFIG:-}
if [ -z "$config_file" ]; then
    config_file=$(sed -n 's/^CELLBRIDGE_CONFIG=//p' "$env_file" | tail -n 1)
fi
config_file=${config_file:-../config.example.yaml}
case "$config_file" in
    /*) ;;
    *) config_file="$compose_dir/$config_file" ;;
esac
if [ ! -f "$config_file" ]; then
    echo "Missing $config_file; set CELLBRIDGE_CONFIG to a deployment config." >&2
    exit 1
fi

if ! tailscale status >/dev/null 2>&1; then
    echo "Tailscale is not authenticated or running; refusing to deploy." >&2
    exit 1
fi
funnel_status=$(tailscale funnel status 2>&1 || true)
if printf '%s\n' "$funnel_status" | grep -Eiq '\(funnel|funnel on|public'; then
    echo "Tailscale Funnel is enabled; disable it before deploying CellBridge." >&2
    exit 1
fi

docker compose --env-file "$env_file" -f "$compose_file" config >/dev/null
docker compose --env-file "$env_file" -f "$compose_file" up -d --build
tailscale serve --bg http://127.0.0.1:8787 >/dev/null
serve_status=$(tailscale serve status 2>&1)
if ! printf '%s\n' "$serve_status" | grep -Fq 'tailnet only'; then
    echo "Tailscale Serve is not tailnet-only; refusing to report success." >&2
    exit 1
fi
if ! printf '%s\n' "$serve_status" | grep -Fq '127.0.0.1:8787'; then
    echo "Tailscale Serve is not proxying to 127.0.0.1:8787." >&2
    exit 1
fi
echo "CellBridge Gateway is starting; check the gateway health endpoint and logs."
