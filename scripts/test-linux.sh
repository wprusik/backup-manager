#!/usr/bin/env bash
# Intended for the disposable test container; starts its private PostgreSQL cluster.
set -euo pipefail
cluster_version=$(pg_lsclusters --no-header | awk 'NR==1 {print $1}')
pg_ctlcluster "$cluster_version" main start
trap 'pg_ctlcluster "$cluster_version" main stop' EXIT
runuser -u postgres -- createuser --superuser root
export TMPDIR=/dev/shm
export GOTMPDIR=/tmp
export BACKUP_MANAGER_INTEGRATION=1
go test -race -count=1 -v ./...
go vet ./...
bash -n scripts/install-systemd.sh
shellcheck scripts/install-systemd.sh scripts/test-linux.sh
systemd-analyze calendar '*-*-* 03:00:00 Europe/Warsaw'
# The executable must exist for systemd's static unit verification.
go build -o /usr/local/bin/backup-manager .
systemd-analyze verify deploy/backup-manager.service deploy/backup-manager.timer
