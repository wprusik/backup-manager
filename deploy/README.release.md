# Install backup-manager

1. Upload this archive from your computer; replace `USER@SERVER` with your SSH login.

```sh
scp backup-manager_@VERSION@_linux_@ARCH@.tar.gz USER@SERVER:~/
ssh USER@SERVER
mkdir -p ~/backup-manager
tar -xzf ~/backup-manager_@VERSION@_linux_@ARCH@.tar.gz -C ~/backup-manager
cd ~/backup-manager
```

2. Install dependencies (Debian/Ubuntu); requires restic >= 0.17.3 and pg_dump >= your PostgreSQL server major version.

```sh
sudo apt-get update
sudo apt-get install -y restic postgresql-client openssh-client
restic version
pg_dump --version
```

3. Set your repository, passwords, host identity, existing paths, exclusions and databases; backend examples are included in the YAML.

```sh
cp config.example.yaml config.yaml
chmod 600 config.yaml
nano config.yaml
```

For PostgreSQL password authentication, use a TCP host such as `127.0.0.1`; set `databases: []` if unused, and keep the repository encryption password off-server.

Hetzner only: authorize root's SSH key for unattended access, enable port 23, and verify the server host key; replace the example account below, then type `quit` to disconnect.

```sh
sudo sftp -P 23 u123456@u123456.your-storagebox.de
```

4. Install the binary, root-owned configuration and systemd units.

```sh
sudo bash scripts/install-systemd.sh ./backup-manager ./config.yaml
sudo backup-manager validate
```

5. Initialize a new repository once; the tmpfs mount is also needed for manual repository commands after a reboot.

```sh
sudo mkdir -p /run/backup-manager-tmp
sudo mount -t tmpfs -o size=256M,mode=0700 tmpfs /run/backup-manager-tmp
sudo backup-manager init
```

6. Run the first backup, then enable daily backups at 03:00 Europe/Warsaw, including after reboot; the service manages its own tmpfs.

```sh
sudo systemctl start backup-manager.service
sudo systemctl enable --now backup-manager.timer
systemctl list-timers backup-manager.timer
sudo journalctl -u backup-manager.service -n 50 --no-pager
```

## Use

```sh
backup-manager help
sudo systemctl start backup-manager.service # Run a backup now.
sudo backup-manager snapshots
sudo backup-manager restore --source files --target /mnt/recovery
sudo backup-manager forget --dry-run # Preview configured retention.
sudo backup-manager forget --prune # Apply retention and free space.
```

Restores preserve original paths under the target directory; retention runs only when you call `forget`.

## Upgrade

Replace only the installed binary from a newer archive; configuration and timer stay in place.

```sh
sudo systemctl stop backup-manager.timer backup-manager.service
sudo install -o root -g root -m 0755 ./backup-manager /usr/local/bin/backup-manager
sudo systemctl start backup-manager.timer
backup-manager version
```
