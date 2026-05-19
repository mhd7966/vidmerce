#!/usr/bin/env bash
# Run golang-migrate with a local binary if installed, otherwise the official
# Docker image (so `make demo` works without `make tools`).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${MIGRATE_IMAGE:-migrate/migrate:v4.18.1}"

if command -v migrate >/dev/null 2>&1; then
	exec migrate "$@"
fi

path=""
database=""
rest=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	-path)
		path="$2"
		shift 2
		;;
	-database)
		database="$2"
		shift 2
		;;
	*)
		rest+=("$1")
		shift
		;;
	esac
done

if [[ -z "$path" || -z "$database" ]]; then
	echo "hack/migrate.sh: -path and -database are required for Docker fallback" >&2
	exit 1
fi

# Reach services published on the host from inside the migrate container.
database="${database//localhost/host.docker.internal}"

abs_path="${path}"
if [[ "$path" != /* ]]; then
	abs_path="${ROOT}/${path}"
fi

echo "migrate: using Docker image ${IMAGE} (install migrate locally or run: make tools)" >&2
exec docker run --rm \
	-v "${abs_path}:/migrations" \
	--add-host=host.docker.internal:host-gateway \
	"${IMAGE}" \
	-path=/migrations \
	-database="${database}" \
	"${rest[@]}"
