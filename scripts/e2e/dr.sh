#!/usr/bin/env bash
set -euo pipefail

export PATH="/usr/local/bin:$PWD:$PATH"

# Every line this script prints is scanned for secrets at the end.
LOG=/tmp/e2e-dr.out
exec > >(tee "$LOG") 2>&1

INST=demo-dev
HOST_NAME=e2e
BUCKET=cvctl-e2e
SECRETS_DIR=/etc/cvctl/secrets
: "${S3_IP:?S3_IP must be set}"
: "${S3_ACCESS:?S3_ACCESS must be set}"
: "${S3_SECRET:?S3_SECRET must be set}"

on_fail() {
  echo "::group::failure diagnostics"
  echo "last res: ${res:-}"
  docker ps -a --format '{{.Names}} {{.Status}}' || true
  docker volume ls -q || true
  for c in $(docker ps -a --filter "label=com.docker.compose.project=${INST}" --format '{{.Names}}'); do
    echo "--- logs $c"; docker logs --tail 60 "$c" 2>&1 || true
  done
  echo "--- bucket"; mcx ls --recursive "ext/${BUCKET}/${HOST_NAME}/${INST}/" 2>&1 | tail -40 || true
  echo "::endgroup::"
}
trap on_fail ERR

# Secret values: masked before first use, never echoed.
WALG_KEY=$(cat "$SECRETS_DIR/walg_libsodium_key")
RESTIC_PW=$(cat "$SECRETS_DIR/restic_password")
PASSPHRASE=$(openssl rand -hex 16)
for v in "$WALG_KEY" "$RESTIC_PW" "$PASSPHRASE" "$S3_SECRET"; do
  test -n "$v"
  echo "::add-mask::$v"
done

mcx() {
  # the silo image ships mc; the external S3 runs it against itself
  docker exec -e MC_HOST_ext="http://${S3_ACCESS}:${S3_SECRET}@127.0.0.1:9000" ext-s3 mc "$@"
}
dbc() {
  docker ps --filter "label=com.docker.compose.project=${INST}" --filter "label=com.docker.compose.service=db" --filter status=running --format '{{.Names}}' | head -n1
}
psql_db() {
  docker exec "$(dbc)" psql -At -c "$1"
}
items() {
  psql_db "select coalesce(string_agg(name, ',' order by id), '') from items"
}
wait_items() {
  local want=$1 got=""
  for _ in $(seq 1 45); do
    got=$(items 2>/dev/null || true)
    [ "$got" = "$want" ] && return 0
    sleep 2
  done
  echo "items: want $want, got $got" >&2
  return 1
}
objs_under() {
  mcx ls --recursive "ext/${BUCKET}/${HOST_NAME}/${INST}/db/$1" 2>/dev/null | wc -l
}

echo "=== 1. Build and push app image, create infra repo ==="
docker build -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop
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

echo "=== 2. project add, rows A and B, backup now, row C ==="
add_res=$(cvctl project add file:///tmp/infra.git --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.ok')" = "true"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
test -n "$(dbc)"
psql_db "insert into items (name) values ('A')" >/dev/null
psql_db "insert into items (name) values ('B')" >/dev/null
res=$(cvctl backup now "$INST" db --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
psql_db "insert into items (name) values ('C')" >/dev/null
test "$(items)" = "A,B,C"
echo "waiting 70s for archive_timeout=60"
sleep 70
test "$(objs_under "")" -gt 0
echo "Assert 1: external bucket holds the gen 0 backup"

echo "=== 3. Stale bundle (gen 0), then db restore -> g1 ==="
rm -f /tmp/bundle.age
CVCTL_SECRETS_PASSPHRASE="$PASSPHRASE" cvctl secrets export -o /tmp/bundle.age --json
test -s /tmp/bundle.age
res=$(cvctl db restore "$INST" db --to latest --json) || { echo "$res"; false; }
echo "$res"
echo "$res" | jq -e '.ok == true'
wait_items "A,B,C"
psql_db "insert into items (name) values ('D')" >/dev/null
res=$(cvctl backup now "$INST" db --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
echo "waiting 70s for archive_timeout=60"
sleep 70
test "$(items)" = "A,B,C,D"
g1=$(objs_under "g1/")
echo "Objects under ${HOST_NAME}/${INST}/db/g1/: $g1"
test "$g1" -gt 0
echo "Assert 2: db restore moved to g1 (A,B,C), D archived under g1"

echo "=== 4. Destroy the host's instance (ext-s3 stays) ==="
systemctl stop cvctl-agent cvctl-backup.timer cvctl-backup-verify.timer || true
docker compose -p "$INST" down -v --remove-orphans || true
docker ps -aq --filter "label=com.docker.compose.project=${INST}" | xargs -r docker rm -f
{
  docker volume ls -q --filter "label=com.docker.compose.project=${INST}"
  docker volume ls -q --filter "name=${INST}"
} | sort -u | xargs -r docker volume rm -f
test -z "$(docker volume ls -q --filter "name=${INST}")"
rm -rf "/srv/deploy/${INST}" "/var/lib/cvctl/${INST}" "$SECRETS_DIR"
test -n "$(docker ps -q --filter name=ext-s3)"
echo "Assert 3: instance, volumes, state and secrets are gone"

echo "=== 5. Runbook: init, import STALE bundle, start agent, project restore ==="
# init needs the s3 credentials as on a real new host; it regenerates the backup keys.
install -d -m 0700 "$SECRETS_DIR"
printf '%s' "$S3_ACCESS" > "$SECRETS_DIR/s3_access_key"
printf '%s' "$S3_SECRET" > "$SECRETS_DIR/s3_secret_key"
chmod 0600 "$SECRETS_DIR/s3_access_key" "$SECRETS_DIR/s3_secret_key"
init_res=$(cvctl init --non-interactive --json)
echo "$init_res" | jq -e '.ok == true and ([.data.steps[] | select(.status == "failed")] | length == 0)'
test "$(cat "$SECRETS_DIR/walg_libsodium_key")" != "$WALG_KEY"
res=$(CVCTL_SECRETS_PASSPHRASE="$PASSPHRASE" cvctl secrets import --force /tmp/bundle.age --json) || { echo "$res"; false; }
echo "$res" | jq -e '.ok == true'
test "$(cat "$SECRETS_DIR/walg_libsodium_key")" = "$WALG_KEY"
# the stale bundle predates any generation switch: GEN is absent or 0
gen=$(sed -n 's/^CVCTL_SVC_DB_BACKUP_GEN=//p' "/srv/deploy/${INST}/cvctl.env")
echo "bundle generation: ${gen:-absent}"
test -z "$gen" -o "$gen" = 0
systemctl start cvctl-agent
res=$(cvctl project restore "$INST" --json) || { echo "$res"; false; }
echo "$res"
echo "$res" | jq -e '.ok == true'
echo "Assert 4: project restore ok"

echo "=== 6. Restore result: stale warning, data from g1, generation 2 ==="
echo "$res" | jq -e '[.data.warnings[]? | select(contains("secrets bundle was stale"))] | length >= 1'
echo "$res" | jq -e '.data.status == "OK"'
wait_items "A,B,C,D"
echo "Assert 5: warning present, items = A,B,C,D (newest data from g1)"
cenv="/srv/deploy/${INST}/cvctl.env"
grep -q '^CVCTL_SVC_DB_BACKUP_GEN=2$' "$cenv"
grep -q '^CVCTL_SVC_DB_BACKUP_GEN_MAX=2$' "$cenv"
echo "Assert 6: cvctl.env names generation 2"
for _ in $(seq 1 30); do
  list=$(cvctl backup list "$INST" db --json || true)
  [ "$(echo "$list" | jq -r '.data.snapshots | length' 2>/dev/null || echo 0)" -ge 1 ] && break
  sleep 2
done
echo "$list"
echo "$list" | jq -e '.data.snapshots | length >= 1'
test "$(objs_under "g2/")" -gt 0
echo "Assert 7: base backup under g2"
test "$(cvctl status --json | jq -r --arg i "$INST" '.data.instances[] | select(.instance==$i) | .status')" = "OK"
greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response: $greeting"
test "$greeting" = "ok hello"
echo "Assert 8: app healthy after restore"

echo "=== 9. No secret value in journal, status, history or this script's output ==="
cvctl status --json > /tmp/leak-status.json
cvctl history "$INST" --json > /tmp/leak-history.json
journalctl --no-pager > /tmp/leak-journal.txt 2>&1 || true
sleep 2
# The ::add-mask:: commands carry the values by design; Actions never prints them.
grep -av "^::add-mask::" "$LOG" > /tmp/leak-script.txt || true
for f in /tmp/leak-status.json /tmp/leak-history.json /tmp/leak-journal.txt /tmp/leak-script.txt; do
  if grep -aqF -e "$WALG_KEY" -e "$RESTIC_PW" -e "$PASSPHRASE" -e "$S3_SECRET" "$f"; then
    echo "secret value found in $f" >&2
    # Show where, with the values replaced, so the leak can be traced.
    WALG_KEY="$WALG_KEY" RESTIC_PW="$RESTIC_PW" PASSPHRASE="$PASSPHRASE" S3_SECRET="$S3_SECRET" python3 - "$f" >&2 <<'PY'
import os, sys
secrets = {k: os.environ[k] for k in ("WALG_KEY", "RESTIC_PW", "PASSPHRASE", "S3_SECRET")}
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
echo "Assert 9: no secret in journal, status, history or script output"

echo "ALL 9 DR ASSERTS PASSED"
