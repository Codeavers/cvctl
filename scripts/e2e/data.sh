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
  echo "last add: ${add_res:-}"
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
add_res=$(cvctl project add file:///tmp/infra.git --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --domain media=media.e2e.local --json)
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

echo "=== 4b. backup now env -> env files snapshot with the host key ==="
res=$(cvctl backup now "$INST" env --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
list=$(cvctl backup list "$INST" env --json)
echo "$list"
echo "$list" | jq -e '[.data.snapshots[] | select(.strategy == "env")] | length == 1'
echo "Assert 1b: env files backed up as one snapshot"

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
# verify covers every backed-up service; media (restic volume) needs a snapshot first.
res=$(cvctl backup now "$INST" media --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
ver=$(cvctl backup verify "$INST" --json) || { echo "$ver"; false; }
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

echo "=== 8b. sqlite: create, write, backup, delete, restore, check ==="
SQ=sq-dev
SQ_IMAGE=ghcr.io/codeavers/cvctl-sqlite:3
sq_helper() {
  local vol=$1
  shift
  docker run --rm --user 1000 -v "$vol:/data" "$SQ_IMAGE" "$@"
}
# A failed run leaves the instance behind. project add refuses it, and its
# repository in the silo stays encrypted with keys the next add replaces. The
# directories go first so the agent stops seeing the instance; the volumes of a
# restore and the leftovers of a killed helper carry no compose label.
sq_reset() {
  rm -rf "/srv/deploy/${SQ}" "/var/lib/cvctl/${SQ}" "/etc/traefik/dynamic/${SQ}.yml"
  docker compose -p "$SQ" down -v --remove-orphans || true
  {
    docker ps -aq --filter "label=com.docker.compose.project=${SQ}"
    docker ps -aq --filter "name=${SQ}"
  } | sort -u | xargs -r docker rm -f
  {
    docker volume ls -q --filter "label=com.docker.compose.project=${SQ}"
    docker volume ls -q --filter "name=${SQ}"
  } | sort -u | xargs -r docker volume rm -f
  test -z "$(docker volume ls -q --filter "name=${SQ}")"
  local silo
  silo=$(docker ps -q --filter label=com.docker.compose.project=cvctl-storage --filter label=com.docker.compose.service=s3)
  test -n "$silo"
  docker exec -i "$silo" sh -s <<SH
set -eu
export MC_HOST_local="http://\${MINIO_ROOT_USER}:\${MINIO_ROOT_PASSWORD}@localhost:9000"
if [ -n "\$(mc ls local/cvctl/${HOST_NAME}/${SQ}/ 2>/dev/null)" ]; then
  mc rm --recursive --force local/cvctl/${HOST_NAME}/${SQ}/
fi
SH
}
sq_reset
rm -rf /tmp/sq-work /tmp/sq.git
mkdir -p /tmp/sq-work
git -C /tmp/sq-work init -b develop
cat > /tmp/sq-work/compose.yml <<'YML'
x-cvctl:
  health: { service: api, http: /health, timeout: 60s }

services:
  api:
    image: localhost:5000/e2e/api
    labels:
      cvctl.role: app
      cvctl.expose: "8080"
    deploy:
      resources:
        limits:
          memory: 128m
    environment:
      STORE_PATH: ${STORE_PATH}
  store:
    labels:
      cvctl.kind: sqlite
      cvctl.owner: api
      cvctl.env: STORE
      cvctl.uid: "1000"
YML
cat > /tmp/sq-work/env.spec.yml <<'YML'
vars:
  STORE_PATH: { source: service, service: store }
YML
git -C /tmp/sq-work config user.name "cvctl-test"
git -C /tmp/sq-work config user.email "test@example.com"
git -C /tmp/sq-work add compose.yml env.spec.yml
git -C /tmp/sq-work commit -m "sqlite infra"
git clone --bare /tmp/sq-work /tmp/sq.git
chmod -R a+rX /tmp/sq.git
(cd /tmp && sudo -u deployer env HOME=/srv/deploy git config --global --replace-all safe.directory /tmp/sq.git '^/tmp/sq\.git$')

add_res=$(cvctl project add file:///tmp/sq.git --name sq --env dev --non-interactive --domain api=sq.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
sq_vol="${SQ}_store-data"
test "$(sq_helper "$sq_vol" stat -c %u /data)" = "1000"

res=$(cvctl set-strategy "$SQ" blue-green --json) && { echo "$res"; false; }
echo "$res" | grep -q E_SQLITE_RECREATE

res=$(cvctl backup now "$SQ" store --json)
echo "$res"
echo "$res" | jq -e '.ok == true and (.data.results[0].note | test("no database"))'
cvctl backup list "$SQ" store --json | jq -e '.data.snapshots | length == 0'

sq_helper "$sq_vol" sqlite3 /data/app.db "pragma journal_mode=wal; create table items (name text); insert into items values ('A')"
res=$(cvctl backup now "$SQ" store --json)
echo "$res"
echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
cvctl backup list "$SQ" store --json | jq -e '.data.snapshots | length == 1'

sq_helper "$sq_vol" rm -f /data/app.db /data/app.db-wal /data/app.db-shm
res=$(cvctl db restore "$SQ" store --to latest --json) || { echo "$res"; false; }
echo "$res"
sq_new=$(echo "$res" | jq -r '.data.volume')
test "$sq_new" != "$sq_vol"
test "$(sq_helper "$sq_new" sqlite3 /data/app.db "select name from items")" = "A"
test "$(sq_helper "$sq_new" stat -c %u /data/app.db)" = "1000"
test "$(cvctl status --json | jq -r --arg i "$SQ" '.data.instances[] | select(.instance==$i) | .status')" = "OK"
docker ps --no-trunc --filter "label=com.docker.compose.project=${SQ}" --filter status=running --format '{{.Mounts}}' | grep -q "$sq_new"
ver=$(cvctl backup verify "$SQ" --json) || { echo "$ver"; false; }
echo "$ver"
echo "$ver" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
echo "Assert 7: sqlite survives a backup, a delete and a restore"

echo "=== 8c. dump engines: create, write, backup, delete, restore, check ==="
# engine_project <name> <kind>: an instance <name>-dev whose service store runs the kind.
engine_project() {
  local name=$1 kind=$2 work=/tmp/$1-work repo=/tmp/$1.git
  rm -rf "$work" "$repo"
  mkdir -p "$work"
  git -C "$work" init -b develop
  cat > "$work/compose.yml" <<YML
x-cvctl:
  health: { service: api, http: /health, timeout: 60s }

services:
  api:
    image: localhost:5000/e2e/api
    labels:
      cvctl.role: app
      cvctl.expose: "8080"
    deploy:
      resources:
        limits:
          memory: 128m
    environment:
      STORE_URL: \${STORE_URL}
  store:
    labels:
      cvctl.kind: $kind
      cvctl.env: STORE
YML
  cat > "$work/env.spec.yml" <<'YML'
vars:
  STORE_URL: { source: service, service: store }
YML
  git -C "$work" config user.name "cvctl-test"
  git -C "$work" config user.email "test@example.com"
  git -C "$work" add compose.yml env.spec.yml
  git -C "$work" commit -m "$kind infra"
  git clone --bare "$work" "$repo"
  chmod -R a+rX "$repo"
  (cd /tmp && sudo -u deployer env HOME=/srv/deploy git config --global --add safe.directory "$repo")
  add_res=$(cvctl project add "file://$repo" --name "$name" --env dev --non-interactive --domain "api=$name.e2e.local" --json)
  echo "$add_res"
  test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
}
store_sh() {
  local c
  c=$(docker ps --filter "label=com.docker.compose.project=$1" --filter label=com.docker.compose.service=store --filter status=running --format '{{.Names}}' | head -n1)
  docker exec "$c" sh -c "$2"
}
# dump_cycle <kind> <write> <wipe> <read>: the scripts run inside the service;
# write stores the row A, wipe loses it and read prints it.
dump_cycle() {
  local kind=$1 write=$2 wipe=$3 read=$4 inst=$1-dev got=
  engine_project "$kind" "$kind"
  store_sh "$inst" "$write"
  test "$(store_sh "$inst" "$read")" = "A"
  res=$(cvctl backup now "$inst" store --json)
  echo "$res"
  echo "$res" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
  cvctl backup list "$inst" store --json | jq -e '.data.snapshots | length == 1'
  store_sh "$inst" "$wipe"
  res=$(cvctl db restore "$inst" store --to latest --json) || { echo "$res"; false; }
  echo "$res"
  for i in $(seq 1 30); do
    got=$(store_sh "$inst" "$read" 2>/dev/null || true)
    [ "$got" = "A" ] && break
    sleep 2
  done
  test "$got" = "A"
  test "$(cvctl status --json | jq -r --arg i "$inst" '.data.instances[] | select(.instance==$i) | .status')" = "OK"
  ver=$(cvctl backup verify "$inst" store --json) || { echo "$ver"; false; }
  echo "$ver"
  echo "$ver" | jq -e '.ok == true and ([.data.results[] | select(.ok != true)] | length == 0)'
  echo "Assert: $kind survives a backup, a delete and a restore"
}

my_sql='MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -N -B -D "$MYSQL_DATABASE"'
dump_cycle mysql \
  "$my_sql -e \"create table items (name varchar(20)); insert into items values ('A')\"" \
  "$my_sql -e 'drop table items'" \
  "$my_sql -e 'select name from items'"

maria_sql='MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -uroot -N -B -D "$MARIADB_DATABASE"'
dump_cycle mariadb \
  "$maria_sql -e \"create table items (name varchar(20)); insert into items values ('A')\"" \
  "$maria_sql -e 'drop table items'" \
  "$maria_sql -e 'select name from items'"

mongo_js() {
  printf '%s' "HOME=/tmp mongosh --nodb --quiet --norc --eval 'const e = process.env; const d = connect(\"mongodb://\" + encodeURIComponent(e.MONGO_INITDB_ROOT_USERNAME) + \":\" + encodeURIComponent(e.MONGO_INITDB_ROOT_PASSWORD) + \"@127.0.0.1:27017/admin\").getSiblingDB(e.MONGO_INITDB_DATABASE); $1'"
}
dump_cycle mongodb \
  "$(mongo_js 'd.items.insertOne({name: "A"}); print("written")')" \
  "$(mongo_js 'd.dropDatabase(); print("dropped")')" \
  "$(mongo_js 'print(d.items.findOne().name)')"

echo "=== 8d. cache and queue engines: start, health, connect ==="
# service_cycle <kind> <url scheme> <connect>: connect runs inside the service and
# prints PONG or OK only with the right credentials.
service_cycle() {
  local kind=$1 scheme=$2 connect=$3 inst=$1-dev c api api_env reply
  engine_project "$kind" "$kind"
  c=$(docker ps --filter "label=com.docker.compose.project=$inst" --filter label=com.docker.compose.service=store --filter status=running --format '{{.Names}}' | head -n1)
  for i in $(seq 1 45); do
    [ "$(docker inspect -f '{{.State.Health.Status}}' "$c")" = "healthy" ] && break
    sleep 2
  done
  test "$(docker inspect -f '{{.State.Health.Status}}' "$c")" = "healthy"
  api=$(docker ps --filter "label=com.docker.compose.project=$inst" --filter label=com.docker.compose.service=api --filter status=running --format '{{.Names}}' | head -n1)
  # grep -q closes the pipe on its first match, and pipefail would fail on the SIGPIPE.
  api_env=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$api")
  grep -q "^STORE_URL=$scheme://" <<<"$api_env"
  reply=$(store_sh "$inst" "$connect")
  grep -qE 'PONG|^OK$' <<<"$reply"
  echo "Assert: $kind is healthy and takes its credentials"
}
service_cycle valkey redis \
  'printf "AUTH %s\r\nPING\r\n" "$VALKEY_PASSWORD" | nc -w 2 127.0.0.1 6379'
service_cycle nats nats \
  'printf "CONNECT {\"auth_token\":\"%s\"}\r\nPING\r\n" "$NATS_TOKEN" | nc -w 2 127.0.0.1 4222'
service_cycle rabbitmq amqp \
  'su-exec rabbitmq rabbitmqctl -q authenticate_user "$RABBITMQ_DEFAULT_USER" "$RABBITMQ_DEFAULT_PASS" >/dev/null && echo OK'

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

echo "ALL 7 DATA ASSERTS PASSED"
