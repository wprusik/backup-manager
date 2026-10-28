#!/usr/bin/env bash
set -euo pipefail

version=${1:-}
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "Usage: $0 vMAJOR.MINOR.PATCH[-PRERELEASE]" >&2
  exit 1
fi
project_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$project_dir"
output_dir="$project_dir/dist/release/$version"
mkdir -p "$output_dir"
staging_dir=$(mktemp -d)
trap 'rm -rf -- "$staging_dir"' EXIT

for arch in amd64 arm64; do
  stage="$staging_dir/$arch"
  mkdir -p "$stage/scripts" "$stage/deploy"
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$stage/backup-manager" .
  install -m 0644 README.md config.example.yaml "$stage/"
  install -m 0755 scripts/install-systemd.sh "$stage/scripts/"
  install -m 0644 deploy/backup-manager.service deploy/backup-manager.timer "$stage/deploy/"
  tar -czf "$output_dir/backup-manager_${version}_linux_${arch}.tar.gz" -C "$stage" .
done

(
  cd "$output_dir"
  sha256sum backup-manager_"$version"_linux_*.tar.gz > SHA256SUMS
)
echo "Release archives: $output_dir"
