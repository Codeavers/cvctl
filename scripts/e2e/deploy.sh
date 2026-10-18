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

echo "=== 2. Create bare git repo /tmp/infra.git on develop branch ==="
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
echo "Assert 2: bare git repo created at /tmp/infra.git with develop branch"

echo "=== 3. Add demo project and verify status OK and /health ==="
add_res=$(cvctl project add file:///tmp/infra.git --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --domain media=media.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.ok')" = "true"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
test "$(jqs '.status')" = "OK"

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
git -C /tmp/infra-work add env.spec.yml
git -C /tmp/infra-work commit -m "add prompt var NEWVAR"
git -C /tmp/infra-work push /tmp/infra.git develop

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

echo "=== 13. Verify systemctl is-active cvctl-agent ==="
active_status=$(systemctl is-active cvctl-agent)
echo "cvctl-agent service status: $active_status"
test "$active_status" = "active"
echo "Assert 13: cvctl-agent systemd unit is active"

echo "ALL 13 ASSERTS PASSED SUCCESSFULLY!"
