# backup-manager

Mały CLI w Go dla Linuksa: restic szyfruje i deduplikuje backup plików oraz
strumieniowanych zrzutów PostgreSQL. Cel: Hetzner Storage Box (SFTP), Cloudflare R2
(S3) albo lokalny katalog. Każde uruchomienie tworzy osobny snapshot plików i po
jednym na bazę, z tagami `backup-manager:ID`, `source:files` / `source:db:NAZWA`
oraz wspólnym `run:...`.

## Instalacja

Wymagane: Linux z systemd i cgroups v2, Go >= 1.23 do kompilacji,
**restic >= 0.17.3**, `pg_dump` (wersja klienta >= wersja serwera), OpenSSH.
Na Debianie/Ubuntu zainstaluj `restic postgresql-client openssh-client`, sprawdzając
wersję restic w swoim wydaniu systemu.

```sh
go build -trimpath -o dist/backup-manager .
cp config.example.yaml config.yaml
# Edytuj config.yaml: host, repozytorium, hasła, istniejące ścieżki i bazy.
chmod 600 config.yaml
sudo bash scripts/install-systemd.sh ./dist/backup-manager ./config.yaml
```

Instalator ustawia właściciela root i prawa 600 konfiguracji, instaluje binarkę
i unity. Nie nadpisuje innej istniejącej konfiguracji. Opcjonalny trzeci argument
to kalendarz systemd; domyślnie codziennie **03:00 Europe/Warsaw**. Timer nadrabia
pominięte wykonanie po uruchomieniu serwera. Usługa działa jako root.

Przed pierwszym użyciem Hetznera skonfiguruj **klucz SSH roota**, dodaj go do
Storage Box i zweryfikuj klucz serwera w `/root/.ssh/known_hosts`. Włącz SSH na
porcie 23 oraz dostęp zewnętrzny, jeśli potrzebny. Sprawdź połączenie:

```sh
sudo sftp -P 23 u123456@u123456.your-storagebox.de
```

Nie wyłączaj weryfikacji klucza hosta. Do automatyzacji użyj klucza dostępnego bez
interaktywnego pytania; dodatkowe opcje SSH ustaw w `/root/.ssh/config`.
Przykłady lokalnego repozytorium i R2 są w YAML. Dla R2 utwórz wcześniej bucket
i klucz API z dostępem do odczytu/zapisu tego bucketa; ustaw region `auto`.
Wybierasz jeden backend na konfigurację.

## Pierwszy backup i harmonogram

Usługa dostaje własny tmpfs automatycznie. Do komend ręcznych przygotuj tmpfs
wskazany w YAML (poniższe polecenie mount wykonaj raz na uruchomienie systemu):

```sh
sudo mkdir -p /run/backup-manager-tmp
sudo mount -t tmpfs -o size=256M,mode=0700 tmpfs /run/backup-manager-tmp
sudo backup-manager validate
sudo backup-manager init                  # tylko raz dla nowego repozytorium
sudo backup-manager backup --dry-run      # również wykonuje pg_dump
sudo systemctl start backup-manager.service
sudo journalctl -u backup-manager.service -n 100
sudo systemctl enable --now backup-manager.timer
systemctl list-timers backup-manager.timer
```

Konfiguracja domyślna: `/etc/backup-manager/config.yaml`. Inną podajesz **przed**
komendą: `backup-manager --config /ścieżka/config.yaml backup`. Konfiguracja
i opcjonalny `password_file` muszą należeć do uruchamiającego użytkownika i mieć
prawa 600. Przechowaj hasło szyfrowania i konfigurację także poza serwerem.
Konto PostgreSQL musi mieć prawo odczytu wszystkich wskazanych danych; root
systemowy sam w sobie nie daje dostępu do PostgreSQL.

## Komendy i odtwarzanie

`help` oraz `KOMENDA --help` opisują opcje. Dostępne komendy:
`version`, `validate`, `init`, `backup`, `snapshots`, `ls`, `restore`, `check`, `forget`.

```sh
sudo backup-manager snapshots --json
sudo backup-manager ls --source files --snapshot latest
sudo backup-manager restore --source files --snapshot latest --target /mnt/recovery
sudo backup-manager restore --source files --snapshot ID --target /mnt/recovery-etc --include /etc
sudo backup-manager restore --source db:app --snapshot latest --target /mnt/recovery-db
sudo backup-manager check --read-data
sudo backup-manager forget --dry-run
sudo backup-manager forget --prune
```

Snapshot ID weź z `snapshots` (zamiast dosłownego `ID`). Plik `/etc/nginx/nginx.conf`
trafi do `/mnt/recovery/etc/nginx/nginx.conf`. Restic odtwarza ścieżki, numeryczne
UID/GID, tryby, mtime, linki, ACL i xattr, o ile system docelowy je obsługuje;
uruchom odtwarzanie jako root i zapewnij zgodne UID/GID użytkowników. `--verify`
jest zawsze włączone. Katalog docelowy musi być pusty lub nieistniejący; świadome
nadpisanie wymaga `--overwrite`. Przywrócenie w oryginalne ścieżki wymaga
`--source files --target / --allow-root --overwrite` (najpierw sprawdź snapshot
w katalogu roboczym). Pliki nieobecne w snapshotcie nie są automatycznie usuwane.

Zrzut bazy to `/mnt/recovery-db/postgres-app.dump`. Import do przygotowanej bazy:

```sh
sudo -u postgres createdb app_recovered
sudo pg_restore --host 127.0.0.1 --username postgres --password \
  --exit-on-error --dbname app_recovered /mnt/recovery-db/postgres-app.dump
```

`pg_restore` odtwarza właścicieli obiektów i granty; role i tablespace muszą już
istnieć. `pg_dump` nie zapisuje globalnych ról/tablespace ani WAL/PITR — te wymagają
osobnej strategii (`pg_dumpall --globals-only`, archiwizacja WAL). Dump nie jest
automatycznie importowany do działającej bazy.

## Pamięć i błędy

Dump nie jest zapisywany na dysku ani w całości ładowany do RAM. Restic czyta
strumień, kompresuje i szyfruje porcje; jego tymczasowe zaszyfrowane packi trafiają
do tmpfs, cache dyskowy jest wyłączony. Domyślnie: pack 16 MiB, 2 połączenia,
1 czytnik, `GOMEMLIMIT=512MiB`; tmpfs usługi ma 256 MiB, a cała usługa limit 1 GiB
i `MemorySwapMax=0`. Go limit jest miękki, a indeks repozytorium rośnie z backupem;
duże repozytoria mogą wymagać zwiększenia limitów. Brak RAM/tmpfs oznacza błąd,
nie nieograniczone zużycie pamięci. Ręczny tmpfs może korzystać ze swapu zgodnie
z ustawieniami systemu. `require_tmpfs: false` świadomie dopuszcza bufory na dysku.

`pg_dump` działa przez `restic --stdin-from-command`: jego błąd nie tworzy snapshotu
bazy. Każdy niezerowy kod restic jest błędem programu/systemd, również kod 3
(niepełny snapshot plików może pozostać w repozytorium). Wcześniejsze snapshoty
danego uruchomienia pozostają po błędzie kolejnego źródła. Po błędzie sprawdź logi
i wybierz sprawdzone ID do restore; `latest` wskazuje najnowszy snapshot źródła,
także potencjalnie niepełny. Snapshoty plików i baz nie są wspólną transakcją;
zatrzymaj zapis aplikacji, jeśli potrzebujesz spójności między nimi. Nie kopiuj
działającego katalogu danych PostgreSQL jako zamiennika dumpu.

Retencja działa wyłącznie po `forget`, dla skonfigurowanego hosta/ID, osobno dla
zestawów ścieżek (czyli także każdej bazy). Backup sam niczego nie usuwa. Restic
nie zapisuje wszystkich metadanych, np. Unix birthtime i flag `chattr`; atime
wymaga `with_atime: true`. Dane zmieniane podczas backupu plików nie tworzą
atomowego obrazu systemu plików.

## Testy

```sh
go test ./...
go vet ./...
docker build -f Dockerfile.test -t backup-manager-tests .
docker run --rm --shm-size=512m backup-manager-tests
```

Kontener uruchamia także testy integracyjne z prawdziwym restic i PostgreSQL:
odtwarzanie danych, UID/GID, uprawnień, mtime, ACL, xattr, linków, deduplikację,
weryfikację repozytorium i brak snapshotu po błędzie dumpu. Hetzner/R2 wymagają
osobnego testu po wpisaniu własnych danych dostępowych.

Dokumentacja: [strumieniowanie i metadane](https://restic.readthedocs.io/en/stable/040_backup.html),
[bufory i pamięć](https://restic.readthedocs.io/en/stable/047_tuning_parameters.html),
[Hetzner](https://docs.hetzner.com/storage/storage-box/access/access-ssh-rsync-borg/),
[R2](https://developers.cloudflare.com/r2/get-started/s3/).
