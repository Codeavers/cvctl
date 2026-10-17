#!/usr/bin/env bash
set -euo pipefail

# Ensure cvctl binary and system tools are available in PATH
export PATH="/usr/local/bin:$PWD:$PATH"

# Helper to read cvctl status --json for an instance
jqs() {
  local filter="$1"
  local inst="${2:-demo-dev}"
  cvctl status "$inst" --json | jq -r "$filter"
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
echo "Assert 2: bare git repo created at /tmp/infra.git with develop branch"

echo "=== 3. Add demo project and verify status OK and /health ==="
add_res=$(cvctl project add file:///tmp/infra.git --name demo --env dev --non-interactive --set GREETING=hello --domain api=api.e2e.local --json)
echo "$add_res"
test "$(echo "$add_res" | jq -r '.ok')" = "true"
test "$(echo "$add_res" | jq -r '.data.status')" = "OK"
test "$(jqs '.data.status')" = "OK"

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

echo "=== 4. Push bad image, run agent --once -> FAILED, /health still returns ok hello ==="
docker build --build-arg MIGRATE_FAIL=1 -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop

cvctl agent --once --json || true
test "$(jqs '.data.status')" = "FAILED"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after failed migration: $greeting"
test "$greeting" = "ok hello"
echo "Assert 4: status FAILED and previous version still answering ok hello"

echo "=== 5. Run agent --once again -> history count unchanged ==="
count_before=$(cvctl history demo-dev --json | jq '.data.history | length')
echo "History count before: $count_before"

cvctl agent --once --json || true

count_after=$(cvctl history demo-dev --json | jq '.data.history | length')
echo "History count after: $count_after"
test "$count_before" -eq "$count_after"
echo "Assert 5: agent --once did not retry failed desired state"

echo "=== 6. Push good image with BUILD_ID=2 -> agent --once -> OK ==="
docker build --build-arg BUILD_ID=2 -t localhost:5000/e2e/api:develop examples/e2e/app
docker push localhost:5000/e2e/api:develop

cvctl agent --once --json
test "$(jqs '.data.status')" = "OK"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after fix: $greeting"
test "$greeting" = "ok hello"
echo "Assert 6: agent deployed new good image and status is OK"

echo "=== 7. Commit prompt var NEWVAR -> agent --once -> BLOCKED; set-env -> agent --once -> OK ==="
cat << 'EOF' >> /tmp/infra-work/env.spec.yml
  NEWVAR: { source: prompt }
EOF
git -C /tmp/infra-work add env.spec.yml
git -C /tmp/infra-work commit -m "add prompt var NEWVAR"
git -C /tmp/infra-work push origin develop

cvctl agent --once --json || true
test "$(jqs '.data.status')" = "BLOCKED"

cvctl set-env demo-dev NEWVAR x --json
cvctl agent --once --json
test "$(jqs '.data.status')" = "OK"
echo "Assert 7: blocked on missing prompt var, then resumed after set-env"

echo "=== 8. Rollback -> PAUSED, Resume -> OK ==="
cvctl rollback demo-dev --json
test "$(jqs '.data.status')" = "PAUSED"

cvctl resume demo-dev --json
test "$(jqs '.data.status')" = "OK"

greeting=$(curl -sk --resolve api.e2e.local:443:127.0.0.1 https://api.e2e.local/health)
echo "Health response after resume: $greeting"
test "$greeting" = "ok hello"
echo "Assert 8: rollback paused instance, resume restored status to OK"

echo "=== 9. Verify systemctl is-active cvctl-agent ==="
active_status=$(systemctl is-active cvctl-agent)
echo "cvctl-agent service status: $active_status"
test "$active_status" = "active"
echo "Assert 9: cvctl-agent systemd unit is active"

echo "ALL 9 ASSERTS PASSED SUCCESSFULLY!"
