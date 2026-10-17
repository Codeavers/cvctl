#!/usr/bin/env bash
set -euo pipefail

export PATH="/usr/local/bin:$PWD:$PATH"

# Every line this script prints is scanned for secrets at the end.
LOG=/tmp/e2e-data.out
exec > >(tee "$LOG") 2>&1

INST=demo-dev
HOST_NAME=e2e

on_fail() {
  echo "::group::failure diagnostics"
  echo "last res: ${res:-}"
  docker ps -a --format '{{.Names}} {{.Status}}' || true
  for c in $(docker ps -a --filter "label=com.docker.compose.project=${INST}" --format '{{.Names}}'); do
    echo "--- logs $c"; docker logs --tail 60 "$c" 2>&1 || true
  done
  echo "::endgroup::"
}
trap on_fail ERR
SECRETS_DIR=/etc/cvctl/secrets

# Secret values: masked before first use, never echoed.
WALG_KEY=$(cat "$SECRETS_DIR/walg_libsodium_key")
RESTIC_PW=$(cat "$SECRETS_DIR/restic_password")
PASSPHRASE=$(openssl rand -hex 16)
for v in "$WALG_KEY" "$RESTIC_PW" "$PASSPHRASE"; do
  test -n "$v"
  echo "::add-mask::$v"
done

dbc() {
  docker ps --filter "label=com.docker.compose.project=${INST}" --filter "label=com.docker.compose.service=db" --filter status=running --format '{{.Names}}' | head -n1
}
psql_db() {
  docker exec "$(dbc)" psql -At -c "$1"
}
items() {
  psql_db "select coalesce(string_agg(name, ',' order by id), '') from items"
}

echo "=== 1. Build and push app image ==="
docker build -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop

echo "=== 2. Create bare git repo /tmp/infra.git ==="
rm -rf /tmp/infra-work /tmp/infra.git
mkdir -p /tmp/infra-work
git -C /tmp/infra-work init -b develop
cp examples/e2e/compose.yml examples/e2e/env.spec.yml /tmp/infra-work/
git -C /tmp/infra-work config user.name "cvctl-test"
git -C /tmp/infra-work config user.email "test@example.com"
git -C /tmp/infra-work add compose.yml env.spec.yml
git -C /tmp/infra-work commit -m "initial infra"
git clone --bare /tmp/infra-work /tmp/infra.git
chmod -R a+rX /tmp/infra.git
(cd /tmp && sudo -u deployer env HOME=/srv/deploy git config --global --add safe.directory /tmp/infra.git)

echo "=== 3. project add with a postgres service ==="
add_res=$(cvctl project add file:///tmp/infra.git --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.ok')" = "true"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
test "$(cvctl status --json | jq -r --arg i "$INST" '.data.instances[] | select(.instance==$i) | .status')" = "OK"
test -n "$(dbc)"
test "$(psql_db "select count(*) from items")" = "0"
echo "Assert 0: project deployed, migrate created table items"

echo "=== 4. backup now -> one full backup in the silo ==="
res=$(cvctl backup now "$INST" db --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
list=$(cvctl backup list "$INST" db --json)
echo "$list"
echo "$list" | jq -e '.data.snapshots | length == 1'
vol=$(docker volume ls -q --filter label=com.docker.compose.project=cvctl-storage --filter label=com.docker.compose.volume=cvctl-storage-data | head -n1)
vol_dir=$(docker volume inspect "$vol" --format '{{.Mountpoint}}')
objs=$(find "$vol_dir" -path "*/${HOST_NAME}/${INST}/db/*" -type f | wc -l)
echo "Objects under ${HOST_NAME}/${INST}/db/: $objs"
test "$objs" -gt 0
echo "Assert 1: one full backup, silo holds objects"

echo "=== 5. Rows A and B around the restore point T ==="
psql_db "insert into items (name) values ('A')" >/dev/null
sleep 2
T=$(date -u +%Y-%m-%dT%H:%M:%SZ)
sleep 2
psql_db "insert into items (name) values ('B')" >/dev/null
test "$(items)" = "A,B"
echo "T=$T; waiting 70s for archive_timeout=60"
sleep 70
echo "Assert 2: rows A,B inserted; T recorded"

echo "=== 6. db restore --to T -> only A ==="
res=$(cvctl db restore "$INST" db --to "$T" --json) || { echo "$res"; false; }
echo "$res"
echo "$res" | jq -e '.ok == true'
prev=$(echo "$res" | jq -r '.data.previous_volume')
test -n "$prev" -a "$prev" != "null"
for i in $(seq 1 30); do
  got=$(items 2>/dev/null || true)
  [ "$got" = "A" ] && break
  sleep 2
done
echo "Items after restore: $got"
test "$got" = "A"
clean=$(cvctl db cleanup "$INST" --json)
echo "$clean"
echo "$clean" | jq -e --arg v "$prev" '.data.removed == false and ([.data.volumes[].name] | index($v)) != null'
echo "Assert 3: PITR restored only A; previous volume still listed"

echo "=== 7. backup verify -> OK, recorded in status ==="
ver=$(cvctl backup verify "$INST" --json)
echo "$ver"
echo "$ver" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
state=$(cat "/var/lib/cvctl/${INST}/state.json")
echo "$state" | jq -e '.backups.db.last_verify_ok != null and (.backups.db.last_verify_error // "") == ""'
echo "Assert 4: verify OK and recorded"

echo "=== 8. secrets export / import into a temp root ==="
rm -rf /tmp/secroot /tmp/secrets.age
CVCTL_SECRETS_PASSPHRASE="$PASSPHRASE" cvctl secrets export -o /tmp/secrets.age --json
test -s /tmp/secrets.age
if grep -aqF -e "$WALG_KEY" -e "$RESTIC_PW" /tmp/secrets.age; then
  echo "plaintext secret in bundle" >&2
  exit 1
fi
imp=$(CVCTL_ALLOW_SECRETS_ROOT=1 CVCTL_SECRETS_PASSPHRASE="$PASSPHRASE" cvctl secrets import --root /tmp/secroot /tmp/secrets.age --json)
n=0
for f in $(echo "$imp" | jq -r '.data.files[]'); do
  cmp "$f" "/tmp/secroot$f"
  n=$((n + 1))
done
echo "Compared $n imported files"
test "$n" -gt 0
cmp "$SECRETS_DIR/walg_libsodium_key" "/tmp/secroot$SECRETS_DIR/walg_libsodium_key"
cmp "$SECRETS_DIR/restic_password" "/tmp/secroot$SECRETS_DIR/restic_password"
echo "Assert 5: exported bundle imports to identical files"

echo "=== 9. No secret value in journal, status, history or this script's output ==="
cvctl status --json > /tmp/leak-status.json
cvctl history "$INST" --json > /tmp/leak-history.json
journalctl --no-pager > /tmp/leak-journal.txt 2>&1 || true
sleep 2
# The ::add-mask:: commands carry the values by design; Actions never prints them.
grep -av "^::add-mask::" "$LOG" > /tmp/leak-script.txt || true
for f in /tmp/leak-status.json /tmp/leak-history.json /tmp/leak-journal.txt /tmp/leak-script.txt; do
  if grep -aqF -e "$WALG_KEY" -e "$RESTIC_PW" -e "$PASSPHRASE" "$f"; then
    echo "secret value found in $f" >&2
    # Show where, with the values replaced, so the leak can be traced.
    WALG_KEY="$WALG_KEY" RESTIC_PW="$RESTIC_PW" PASSPHRASE="$PASSPHRASE" python3 - "$f" >&2 <<'PY'
import os, sys
secrets = {k: os.environ[k] for k in ("WALG_KEY", "RESTIC_PW", "PASSPHRASE")}
for n, line in enumerate(open(sys.argv[1], errors="replace"), 1):
    hits = [k for k, v in secrets.items() if v and v in line]
    if hits:
        for k in hits:
            line = line.replace(secrets[k], "<" + k + ">")
        print(f"{n}: {','.join(hits)}: {line.rstrip()[:400]}")
PY
    exit 1
  fi
done
echo "Assert 6: no secret in journal, status, history or script output"

echo "ALL 6 DATA ASSERTS PASSED"
