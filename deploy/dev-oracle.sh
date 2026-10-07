#!/usr/bin/env bash
# Start a throw-away Oracle Free container with the HR fixture for integration tests.
# Writes connection settings to .ovc-dev.env (gitignored); `source` it before `go test`.
set -euo pipefail
cd "$(dirname "$0")/.."
NAME=${OVC_DEV_ORACLE_NAME:-ovc-oracle}
PORT=${OVC_DEV_ORACLE_PORT:-15210}
rnd() { openssl rand -hex 6; }

if docker ps -a --format '{{.Names}}' | grep -qx "$NAME"; then
  echo "container $NAME already exists (docker rm -f $NAME to recreate)"; exit 1
fi
SYS_PW="Sys_$(rnd)"; HR_PW="Hr_$(rnd)"; RD_PW="Rd_$(rnd)"
docker run -d --name "$NAME" -p "$PORT:1521" -e ORACLE_PASSWORD="$SYS_PW" gvenzl/oracle-free:23-slim >/dev/null
echo -n "waiting for database"
until docker logs "$NAME" 2>&1 | grep -q "DATABASE IS READY TO USE"; do echo -n .; sleep 5; done; echo
docker cp sql/dev/fixture.sql "$NAME:/tmp/fixture.sql"
docker exec "$NAME" bash -c "sqlplus -s sys/$SYS_PW@//localhost:1521/FREEPDB1 as sysdba @/tmp/fixture.sql '$HR_PW' '$RD_PW'" >/dev/null
umask 077
cat > .ovc-dev.env <<ENV
export OVC_IT_HOST=localhost
export OVC_IT_PORT=$PORT
export OVC_IT_SERVICE=FREEPDB1
export OVC_IT_USER=OVC_READER
export OVC_IT_PASSWORD=$RD_PW
export OVC_IT_SCHEMA=HR
export OVC_IT_OWNER_PASSWORD=$HR_PW
ENV
echo "ready: source .ovc-dev.env && go test ./internal/oracle/..."
