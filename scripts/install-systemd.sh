#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ ${EUID} -ne 0 ]]; then
  echo "Run as root: sudo bash scripts/install-systemd.sh BINARY CONFIG [CALENDAR]" >&2
  exit 1
fi
if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "Usage: $0 BINARY CONFIG [CALENDAR]" >&2
  exit 1
fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
binary=$(realpath -- "$1")
config=$(realpath -- "$2")
calendar=${3:-'*-*-* 03:00:00 Europe/Warsaw'}
if [[ "$calendar" == *$'\n'* || "$calendar" == *$'\r'* ]]; then
  echo "Calendar must be a single line" >&2
  exit 1
fi
systemd-analyze calendar "$calendar" >/dev/null
[[ -f "$binary" && -f "$config" ]]
"$binary" version
for dependency in restic pg_dump ssh mount; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done

config_dest=/etc/backup-manager/config.yaml
# Do not silently replace an existing deployment's secrets.
if [[ -e "$config_dest" && "$config" != "$config_dest" ]] && ! cmp -s -- "$config" "$config_dest"; then
  echo "Existing $config_dest differs. Edit it explicitly, then pass that path." >&2
  exit 1
fi
install -d -o root -g root -m 0700 /etc/backup-manager /run/backup-manager
install -o root -g root -m 0755 "$binary" /usr/local/bin/backup-manager
if [[ "$config" != "$config_dest" ]]; then
  install -o root -g root -m 0600 "$config" "$config_dest"
else
  chown root:root "$config_dest"
  chmod 0600 "$config_dest"
fi
/usr/local/bin/backup-manager --config "$config_dest" validate
install -o root -g root -m 0644 "$script_dir/../deploy/backup-manager.service" /etc/systemd/system/backup-manager.service
{
  printf '[Unit]\nDescription=Scheduled server backup\n\n[Timer]\n'
  printf 'OnCalendar=%s\n' "$calendar"
  printf 'Persistent=true\nAccuracySec=1min\nUnit=backup-manager.service\n\n[Install]\nWantedBy=timers.target\n'
} >/etc/systemd/system/backup-manager.timer
systemd-analyze verify /etc/systemd/system/backup-manager.service /etc/systemd/system/backup-manager.timer
systemctl daemon-reload
echo "Installed. Initialize and test the repository using README, then enable scheduling:"
echo "  systemctl enable --now backup-manager.timer"
