#!/usr/bin/env bash
set -euo pipefail

# Ensure cvctl binary and system tools are available in PATH
export PATH="/usr/local/bin:$PWD:$PATH"

# Helper to read cvctl status --json for an instance
jqs() {
  local filter="$1"
  local inst="${2:-demo-dev}"
  cvctl status --json | jq -r --arg i "$inst" ".data.instances[] | select(.instance==\$i) | $filter"
}

echo "=== 1. Build and push good image ==="
docker build -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop
echo "Assert 1: image built and pushed"

echo "=== 2. Publish the contract to the local registry with cvctl publish ==="
INFRA_REF=localhost:5000/e2e/infra
rm -rf /tmp/infra-work && mkdir -p /tmp/infra-work
cp examples/e2e/compose.yml examples/e2e/env.spec.yml /tmp/infra-work/
pub=$(cvctl publish /tmp/infra-work "$INFRA_REF:develop" --json)
echo "$pub"
test "$(echo "$pub" | jq -r '.ok')" = "true"
echo "$pub" | jq -r '.data.digest' | grep -qE '^sha256:[0-9a-f]{64}$'
echo "Assert 2: contract published as an OCI artifact"

echo "=== 3. Add demo project and verify status OK and /health ==="
add_res=$(cvctl project add "oci://$INFRA_REF" --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --domain media=media.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.ok')" = "true"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
test "$(jqs '.status')" = "OK"
grep -qx 'CVCTL_INFRA_SOURCE=oci' /srv/deploy/demo-dev/cvctl.env
test ! -e /var/lib/cvctl/demo-dev/deploy_key

greeting=""
for i in $(seq 1 15); do
  greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health || true)
  if [ "$greeting" = "ok hello" ]; then
    break
  fi
  sleep 1
done
echo "Health response: $greeting"
test "$greeting" = "ok hello"
echo "Assert 3: status OK and /health returned ok hello"

echo "=== 4. app.env carries the scoped app key, never the silo root password ==="
APP_ENV=/srv/deploy/demo-dev/app.env
root_pw=$(grep '^CVCTL_SVC_MEDIA_PASSWORD=' /srv/deploy/demo-dev/cvctl.env | cut -d= -f2-)
test -n "$root_pw"
grep -qx 'S3_ACCESS_KEY=media-app' "$APP_ENV"
if grep -qF "$root_pw" "$APP_ENV"; then echo "root password leaked into app.env"; exit 1; fi
echo "Assert 4: app.env has S3_ACCESS_KEY=media-app and no root password"

echo "=== 5. The app key writes to both buckets and is not an admin ==="
MEDIA=$(docker ps -q --filter label=com.docker.compose.project=demo-dev --filter label=com.docker.compose.service=media)
test -n "$MEDIA"
app_ak=$(grep '^S3_ACCESS_KEY=' "$APP_ENV" | cut -d= -f2-)
app_sk=$(grep '^S3_SECRET_KEY=' "$APP_ENV" | cut -d= -f2-)
printf 'set -e\nmc alias set app http://localhost:9000 %s %s >/dev/null\necho hello-pub | mc pipe app/pub/e2e.txt >/dev/null\necho hello-priv | mc pipe app/priv/e2e.txt >/dev/null\nif mc admin user list app >/dev/null 2>&1; then echo "app key has admin rights"; exit 1; fi\n' "$app_ak" "$app_sk" \
  | docker exec -i "$MEDIA" sh -s
echo "Assert 5: app key put into pub and priv; admin API denied"

echo "=== 6. Public route: GET/HEAD on public objects only ==="
code() { curl -sk -o /dev/null -w '%{http_code}' --resolve media.e2e.local:443:127.0.0.1 "$@"; }
body=$(curl -sk --resolve media.e2e.local:443:127.0.0.1 https://media.e2e.local/pub/e2e.txt)
test "$body" = "hello-pub"
test "$(code https://media.e2e.local/pub/e2e.txt)" = "200"
test "$(code -I https://media.e2e.local/pub/e2e.txt)" = "200"
test "$(code https://media.e2e.local/priv/e2e.txt)" = "404"
test "$(code https://media.e2e.local/pub/)" = "404"
test "$(code https://media.e2e.local/)" = "404"
test "$(code -X PUT --data x https://media.e2e.local/pub/x)" = "404"
echo "Assert 6: public GET/HEAD 200; private, listing, root and PUT 404"

echo "=== 7. A second deploy re-runs provisioning without errors ==="
redeploy=$(cvctl deploy demo-dev --json)
echo "$redeploy"
test "$(echo "$redeploy" | jq -r '.ok')" = "true"
test "$(jqs '.status')" = "OK"
echo "Assert 7: second deploy OK (provisioning is idempotent)"

echo "=== 8. Push bad image, run agent --once -> FAILED, /health still returns ok hello ==="
docker build --build-arg MIGRATE_FAIL=1 -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop

(cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json) || true
test "$(jqs '.status')" = "FAILED"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after failed migration: $greeting"
test "$greeting" = "ok hello"
echo "Assert 8: status FAILED and previous version still answering ok hello"

echo "=== 9. Run agent --once again -> history count unchanged ==="
count_before=$(cvctl history demo-dev --json | jq '.data.history | length')
echo "History count before: $count_before"

(cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json) || true

count_after=$(cvctl history demo-dev --json | jq '.data.history | length')
echo "History count after: $count_after"
test "$count_before" -eq "$count_after"
echo "Assert 9: agent --once did not retry failed desired state"

echo "=== 10. Push good image with BUILD_ID=2 -> agent --once -> OK ==="
docker build --build-arg BUILD_ID=2 -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop

(cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json)
test "$(jqs '.status')" = "OK"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after fix: $greeting"
test "$greeting" = "ok hello"
echo "Assert 10: agent deployed new good image and status is OK"

echo "=== 11. Commit prompt var NEWVAR -> agent --once -> BLOCKED; set-env -> agent --once -> OK ==="
cat << 'EOF' >> /tmp/infra-work/env.spec.yml
  NEWVAR: { source: prompt }
EOF
cvctl publish /tmp/infra-work "$INFRA_REF:develop" --json

(cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json) || true
test "$(jqs '.status')" = "BLOCKED"

cvctl set-env demo-dev NEWVAR x --json
(cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json)
test "$(jqs '.status')" = "OK"
echo "Assert 11: blocked on missing prompt var, then resumed after set-env"

echo "=== 12. Rollback -> PAUSED, Resume -> OK ==="
cvctl rollback demo-dev --json
test "$(jqs '.status')" = "PAUSED"

cvctl resume demo-dev --json
test "$(jqs '.status')" = "OK"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after resume: $greeting"
test "$greeting" = "ok hello"
echo "Assert 12: rollback paused instance, resume restored status to OK"

AGENT_ONCE() { (cd /tmp && sudo -u deployer --preserve-env=CVCTL_INSECURE_REGISTRIES,CVCTL_ALLOW_FILE_REPOS /usr/local/bin/cvctl agent --once --json); }
cur_digest() { jq -r ".current.$2" "/var/lib/cvctl/$1/state.json"; }

echo "=== 13. A forbidden contract is rejected by publish and, pushed raw, by the agent ==="
rm -rf /tmp/evil && cp -r /tmp/infra-work /tmp/evil
sed -i 's|^  api:$|  api:\n    privileged: true\n    volumes: ["/:/host"]|' /tmp/evil/compose.yml
grep -q 'privileged: true' /tmp/evil/compose.yml
if evil=$(cvctl publish /tmp/evil "$INFRA_REF:develop" --json); then echo "publish accepted a forbidden contract"; exit 1; fi
echo "$evil" | grep -q E_CONTRACT_FIELD_FORBIDDEN
good_digest=$(cur_digest demo-dev infra_digest)
./rawpush /tmp/evil "$INFRA_REF:develop"
agent_out=$(AGENT_ONCE 2>&1 || true)
echo "$agent_out" | grep -q E_CONTRACT_FIELD_FORBIDDEN
test "$(cur_digest demo-dev infra_digest)" = "$good_digest"
test "$(jqs '.status')" = "OK"
test -z "$(docker ps -q --filter label=com.docker.compose.project=demo-dev --filter label=com.docker.compose.service=api --format '{{.ID}}' | xargs -r docker inspect -f '{{.HostConfig.Privileged}}' | grep true)"
cvctl publish /tmp/infra-work "$INFRA_REF:develop" --json
echo "Assert 13: forbidden contract rejected by publish and by the agent; running contract unchanged"

echo "=== 14. A second instance cannot reach the first ==="
AUX_REF=localhost:5000/e2e/infra-aux
rm -rf /tmp/aux-work && cp -r /tmp/infra-work /tmp/aux-work
cvctl publish /tmp/aux-work "$AUX_REF:develop" --json
aux_res=$(cvctl project add "oci://$AUX_REF" --name aux --env dev --non-interactive --set GREETING=aux --set NEWVAR=x --domain api=apiaux.e2e.local --domain media=mediaaux.e2e.local --json)
echo "$aux_res"
test "$(echo "$aux_res" | jq -r '.ok')" = "true"
demo_db=$(docker ps -q --filter label=com.docker.compose.project=demo-dev --filter label=com.docker.compose.service=db)
db_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$demo_db" | awk '{print $1}')
test -n "$db_ip"
if docker run --rm --network aux-dev_edge alpine:3 getent hosts demo-dev--api; then echo "aux resolves demo-dev--api"; exit 1; fi
for net in aux-dev_edge aux-dev_data aux-dev_backup; do
  if docker run --rm --network "$net" alpine:3 nc -z -w 3 "$db_ip" 5432; then echo "$net reaches demo-dev db"; exit 1; fi
done
rm -rf /tmp/aux-hijack && cp -r /tmp/aux-work /tmp/aux-hijack
sed -i 's|^services:$|services:\n  demo-dev--api:\n    image: localhost:5000/e2e/api\n    labels: { cvctl.role: app, cvctl.expose: "8080" }\n    deploy: { resources: { limits: { memory: 64m } } }|' /tmp/aux-hijack/compose.yml
grep -q '^  demo-dev--api:$' /tmp/aux-hijack/compose.yml
if hijack=$(cvctl publish /tmp/aux-hijack "$AUX_REF:develop" --json); then echo "publish accepted service demo-dev--api"; exit 1; fi
echo "$hijack" | grep -q E_CONTRACT_SERVICE_NAME
greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
test "$greeting" = "ok hello"
echo "Assert 14: aux-dev neither resolves demo-dev's alias nor reaches its database; a service named demo-dev--api is rejected"

echo "=== 15. An instance's backup user cannot read another instance's backups ==="
cvctl backup now demo-dev db --json | jq -e '.ok == true'
cvctl backup now aux-dev db --json | jq -e '.ok == true'
SILO=$(docker ps -q --filter label=com.docker.compose.project=cvctl-storage --filter label=com.docker.compose.service=s3)
aux_ak=$(sed -n 's/^CVCTL_BACKUP_ACCESS_KEY=//p' /srv/deploy/aux-dev/cvctl.env)
aux_sk=$(sed -n 's/^CVCTL_BACKUP_SECRET_KEY=//p' /srv/deploy/aux-dev/cvctl.env)
test -n "$aux_ak" && test -n "$aux_sk"
printf 'export MC_HOST_x="http://%s:%s@localhost:9000"\nmc ls x/cvctl/e2e/aux-dev/ >/dev/null\nif mc ls x/cvctl/e2e/demo-dev/ >/dev/null 2>&1; then echo "aux lists demo-dev backups"; exit 1; fi\n' "$aux_ak" "$aux_sk" \
  | docker exec -i "$SILO" sh -s
grep -q "$aux_sk" /srv/deploy/aux-dev/app.env && { echo "backup secret in app.env"; exit 1; } || true
echo "Assert 15: aux-dev backup user lists its own prefix only"

echo "=== 16. A hook that hangs times out without blocking the other instance ==="
sed -i 's|run: "/app migrate"|run: "sleep 60", timeout: 5s|' /tmp/aux-work/compose.yml
grep -q 'timeout: 5s' /tmp/aux-work/compose.yml
cvctl publish /tmp/aux-work "$AUX_REF:develop" --json
api_before=$(cur_digest demo-dev 'digests.api')
docker build --build-arg BUILD_ID=3 -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop
AGENT_ONCE || true
cvctl history aux-dev --json | jq -e '[.data.history[] | (.detail // "") | select(contains("E_TIMEOUT"))] | length >= 1'
test "$(jqs '.status' aux-dev)" = "FAILED"
test "$(jqs '.status')" = "OK"
test "$(cur_digest demo-dev 'digests.api')" != "$api_before"
aux_events=$(cvctl history aux-dev -n 1000 --json | jq '.data.history | length')
AGENT_ONCE || true
test "$(cvctl history aux-dev -n 1000 --json | jq '.data.history | length')" = "$aux_events"
test "$(jqs '.status' aux-dev)" = "FAILED"
echo "Assert 16: aux-dev hook timed out (E_TIMEOUT) and is not retried for the same desired state; demo-dev deployed the new image in the same tick"

echo "=== 17. Verify systemctl is-active cvctl-agent ==="
active_status=$(systemctl is-active cvctl-agent)
echo "cvctl-agent service status: $active_status"
test "$active_status" = "active"
echo "Assert 17: cvctl-agent systemd unit is active"

echo "ALL 17 ASSERTS PASSED SUCCESSFULLY!"
