#!/bin/sh
# Invoked over SSH by deploy.ps1. Requires root or passwordless sudo.
set -eu
archive=${1:?archive path required}
envfile=${2:?environment file path required}
destination=${3:-/opt/mp-panel}
case "$archive" in /tmp/mp-panel-*.tar.gz) ;; *) echo 'Invalid source archive' >&2; exit 1;; esac
case "$envfile" in /tmp/mp-panel-*.env) ;; *) echo 'Invalid environment upload' >&2; exit 1;; esac
case "$destination" in /*) ;; *) echo 'Destination must be absolute' >&2; exit 1;; esac
case "$destination" in /|/etc|/usr|/var|/home|/root|*/../*|*/..) echo 'Choose a dedicated installation directory' >&2; exit 1;; esac
if [ "$(id -u)" -ne 0 ]; then echo 'Installer requires root or passwordless sudo' >&2; exit 1; fi
trap 'rm -f -- "$archive" "$envfile"' EXIT
if [ -e "$destination/.env" ]; then
  echo 'Existing installation detected. Use setup.sh --source with this directory for a backed-up update.' >&2
  exit 1
fi
if [ ! -f "$archive" ] || [ ! -f "$envfile" ]; then echo 'Missing staged source or .env' >&2; exit 1; fi
if ! command -v docker >/dev/null 2>&1; then
  # Official convenience installer; changes system packages on this deployment target.
  if ! command -v curl >/dev/null 2>&1; then
    if command -v apt-get >/dev/null 2>&1; then apt-get update; apt-get install -y ca-certificates curl;
    else echo 'Install curl and Docker Engine with Compose first' >&2; exit 1; fi
  fi
  docker_script=$(mktemp /tmp/mp-docker-install.XXXXXX)
  curl -fsSL https://get.docker.com -o "$docker_script"
  if ! sh "$docker_script"; then rm -f -- "$docker_script"; exit 1; fi
  rm -f -- "$docker_script"
fi
docker compose version >/dev/null
if [ -L "$destination" ]; then echo 'Installation directory cannot be a symlink' >&2; exit 1; fi
mkdir -p -- "$destination"
# Uploaded archive is created from the local project by the companion deploy tool.
tar -xzf "$archive" -C "$destination" --no-same-owner
install -m 600 "$envfile" "$destination/.env"
cd "$destination"
set -- docker compose -f compose.yaml
if grep -Eq '^MP_NPM_NETWORK=.+$' .env; then set -- "$@" -f deploy/compose.npm.yaml; fi
if grep -Eq '^MP_ENABLE_GATEWAY=true[[:space:]]*$' .env; then
  if grep -Eq '^MP_GATEWAY_PROVIDER=mc-router[[:space:]]*$' .env; then
    set -- "$@" -f deploy/compose.mc-router.yaml
  else
    set -- "$@" -f deploy/compose.gate.yaml
  fi
fi
"$@" up -d --build --wait --wait-timeout 180
echo "MP installed in $destination"
echo 'Read setup token: docker compose exec -T api cat /var/lib/mp/keys/setup.token'
