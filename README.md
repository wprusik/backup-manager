# backup-manager

Linux CLI for encrypted, incremental file and PostgreSQL backups using restic.
Supports Hetzner Storage Box (SFTP), Cloudflare R2 (S3), and local repositories.

## Releases

GitHub Actions runs only on version tags: tests, then publishes Linux amd64/arm64
archives (binary, installer, config, SHA256 checksums). Commit and push changes first:

```sh
git tag v0.1.2
git push origin v0.1.2
```

Download and extract your architecture's archive from GitHub Releases. Use
`./backup-manager` instead of `./dist/backup-manager` in the install command below.
The archive includes a short [installation guide](deploy/README.release.md) as `README.md`.

## Setup

Requires Linux/systemd, cgroups v2, restic >= 0.17.3, OpenSSH, and `pg_dump`
(client major version >= server). Build with Go >= 1.23.

```sh
go build -trimpath -o dist/backup-manager .
cp config.example.yaml config.yaml
# Edit repository credentials, host, paths, exclusions, databases, and retention.
chmod 600 config.yaml
sudo bash scripts/install-systemd.sh ./dist/backup-manager ./config.yaml

# Manual commands need this tmpfs; the systemd service creates its own.
sudo mkdir -p /run/backup-manager-tmp
sudo mount -t tmpfs -o size=256M,mode=0700 tmpfs /run/backup-manager-tmp
sudo backup-manager validate
sudo backup-manager init # Once per repository.
sudo systemctl start backup-manager.service
sudo systemctl enable --now backup-manager.timer
```

Default config: `/etc/backup-manager/config.yaml` (root-owned, mode 600).
Override with `backup-manager --config /path/config.yaml COMMAND`.
The service runs as root daily at **03:00 Europe/Warsaw**.
Pass a systemd calendar as the installer's third argument to change it.

- **Hetzner:** configure root's SSH key and verified `known_hosts`; enable port 23.
- **R2:** create a bucket and read/write API credentials; use region `auto`.
- Backend examples are in `config.example.yaml`. Keep the encryption password off-server.

## Commands

```sh
backup-manager help # Also: COMMAND --help
sudo backup-manager backup
sudo backup-manager snapshots --json
sudo backup-manager ls --source files
sudo backup-manager restore --source files --target /mnt/recovery
sudo backup-manager restore --source db:app --target /mnt/recovery-db
sudo backup-manager check --read-data
sudo backup-manager forget --dry-run
sudo backup-manager forget --prune
sudo journalctl -u backup-manager.service -n 100
```

Restore defaults to `latest`; use `--snapshot ID` for a specific snapshot.
`/etc/example` restores to `/mnt/recovery/etc/example`, preserving numeric UID/GID,
permissions, mtime, links, ACLs, and xattrs where supported. Run as root.
Content verification is automatic. Nonempty targets require `--overwrite`;
restoring directly to `/` also requires `--allow-root`.

Database restore extracts `postgres-app.dump`. Import into an existing database:

```sh
sudo pg_restore -h 127.0.0.1 -U postgres -W --exit-on-error \
  --dbname app_recovered /mnt/recovery-db/postgres-app.dump
```

Database roles/tablespaces must already exist; global objects and WAL/PITR are not backed up.

## Behavior

- Dumps stream through `--stdin-from-command`; failed dumps create no DB snapshot.
- Temporary encrypted packs use tmpfs; disk cache is disabled. Service limits: 256 MiB tmpfs, 1 GiB RAM, no swap. Large repositories may need higher limits.
- Files and each database have separate snapshots. Runs are not atomic; any nonzero exit is failure. After errors, inspect logs before using `latest`: incomplete file snapshots can remain.
- Retention runs only through `forget`; backups never delete old snapshots.

## Tests

```sh
go test ./...
go vet ./...
# Linux integration tests with real restic and PostgreSQL:
docker build -f Dockerfile.test -t backup-manager-tests .
docker run --rm --shm-size=512m backup-manager-tests
```
