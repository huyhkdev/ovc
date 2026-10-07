#!/usr/bin/env bash
# Local playground: OVC Server + Oracle (Docker) + a git remote.
#
#   deploy/dev-oracle.sh                      # once: start Oracle with the HR fixture
#   deploy/dev.sh server                      # terminal 1: run ovc-server (foreground)
#   deploy/dev.sh ovc init HR --db dev        # terminal 2: admin commands (init, schemas)
#   deploy/dev.sh ovc schemas
#   /path/to/ovc/bin/ovc get HR.PKG_X         # in a git clone of the repo (dev commands)
# bin/ovc is built with this playground's server inside; your identity comes
# from git config user.name / user.email.
#   deploy/dev.sh inspect                     # what is in the remote now
#   deploy/dev.sh checkout [branch]           # real files to browse: <work>/view/<branch> (default dev)
#   deploy/dev.sh reset                       # forget what was created in this mode (keeps Oracle)
#
# Two modes, each with its own state folder:
#   local  (default)    remote = a bare repo on disk          -> .ovc-dev/
#   github              OVC_DEV_REMOTE=<url> OVC_GIT_TOKEN=... -> .ovc-dev/github/
# In github mode the CI files (.github/workflows, .gitlab-ci.yml) are NOT written
# unless OVC_DEV_CI=1, so no token with "workflow" permission is needed and no
# failing GitHub Actions runs appear in a test repo.
set -euo pipefail
cd "$(dirname "$0")/.."
PORT=${OVC_DEV_PORT:-18080}
REMOTE_URL=${OVC_DEV_REMOTE:-}
if [ -n "$REMOTE_URL" ]; then
  W="$PWD/.ovc-dev/github"; MODE=github
else
  W="$PWD/.ovc-dev"; MODE=local
fi
# where `inspect` and `checkout` read branches from
if [ "$MODE" = github ]; then SRC="$W/mirror.git"; else SRC="$W/remote.git"; fi

need_env() {
  [ -f .ovc-dev.env ] || { echo "no .ovc-dev.env: run deploy/dev-oracle.sh first" >&2; exit 1; }
  # shellcheck disable=SC1091
  source .ovc-dev.env
}
src_git() { GIT_DIR="$SRC" GIT_CONFIG_GLOBAL=/dev/null git "$@"; }

case "${1:-}" in
server)
  need_env
  mkdir -p "$W"
  go build -ldflags "-X ovc/internal/cli.DefaultServer=http://127.0.0.1:$PORT" -o bin/ ./cmd/...
  CI_BLOCK=""
  if [ "$MODE" = github ]; then
    if [ -z "${OVC_GIT_TOKEN:-}" ]; then
      echo "github mode needs a token in your shell (never paste it in a chat):" >&2
      echo "  export OVC_GIT_TOKEN=...   # fine-grained, only this repo: Contents + Pull requests, read and write" >&2
      exit 1
    fi
    GIT_REMOTE="$REMOTE_URL"
  else
    [ -d "$W/remote.git" ] || git init --bare --quiet "$W/remote.git"
    GIT_REMOTE="$W/remote.git"
  fi
  if [ "${OVC_DEV_CI:-0}" = 1 ] || [ "$MODE" = local ]; then
    CI_BLOCK='ci:
  github_template: "your-org/ovc-ci-templates/.github/workflows/oracle-deploy.yml@v1"
  gitlab_project: "db/ovc-ci-templates"'
  fi
  cat > "$W/ovc-server.yaml" <<YAML
listen: "127.0.0.1:$PORT"
git:
  host: github
  remote: "$GIT_REMOTE"
  token_env: OVC_GIT_TOKEN
  committer: { name: "OVC Bot", email: "ovc-bot@company.local" }
envs: [dev, prd]
$CI_BLOCK
storage:
  mirror_dir: "$W/mirror.git"
  state_file: "$W/state.json"
databases:
  dev: { dsn: "$OVC_IT_HOST:$OVC_IT_PORT/$OVC_IT_SERVICE", user: "$OVC_IT_USER", password_env: OVC_DB_PASSWORD }
  prd: { dsn: "$OVC_IT_HOST:$OVC_IT_PORT/$OVC_IT_SERVICE", user: "$OVC_IT_USER", password_env: OVC_DB_PASSWORD }
cli:
  min_version: 0.1.0
YAML
  export OVC_DB_PASSWORD="$OVC_IT_PASSWORD"
  echo "mode=$MODE remote=$GIT_REMOTE state=$W/state.json url=http://127.0.0.1:$PORT ci_files=$([ -n "$CI_BLOCK" ] && echo yes || echo no)"
  exec bin/ovc-server -config "$W/ovc-server.yaml"
  ;;
ovc)
  shift
  [ -x bin/ovc ] || { echo "run deploy/dev.sh server first (it builds bin/ovc)" >&2; exit 1; }
  exec bin/ovc "$@"
  ;;
inspect)
  [ -d "$SRC" ] || { echo "nothing yet: run init first"; exit 0; }
  echo "== mode=$MODE, reading $SRC"
  echo "== branches"; src_git for-each-ref --format='%(refname:short)  %(objectname:short)  %(authorname) %(authoremail)  committer=%(committername)' refs/heads
  for b in $(src_git for-each-ref --format='%(refname:short)' refs/heads); do
    echo; echo "== branch $b (= db $b): files per owner folder"
    src_git ls-tree -r --name-only "$b" | awk -F/ '$1 ~ /^\./ || NF==1 {c["(root: CI files)"]++; next} {c[$1"/"]++} END{for(k in c) printf "  %-22s %d\n", k, c[k]}' | sort
    echo "== history of $b"
    src_git log --graph --format='%h %s  [%an]' "$b" | sed 's/^/  /'
  done
  b=$(src_git for-each-ref --format='%(refname:short)' refs/heads | head -1)
  if [ -n "$b" ]; then
    echo; echo "== sample stub ($b)"
    f=$(src_git ls-tree -r --name-only "$b" | grep -m1 '/packages/') || true
    [ -n "${f:-}" ] && { echo "$f:"; src_git show "$b:$f"; }
    for y in $(src_git ls-tree -r --name-only "$b" | grep '^[^./][^/]*/ovc.yaml$'); do
      echo; echo "== $y ($b)"; src_git show "$b:$y"
    done
    echo "== last commit ($b)"; src_git log -1 --format='%an <%ae>%n%s%n%(trailers)' "$b"
  fi
  ;;
checkout)
  b=${2:-dev}
  [ -d "$SRC" ] || { echo "nothing yet: run init first" >&2; exit 1; }
  rm -rf "$W/view/$b"; mkdir -p "$W/view"
  git clone --quiet --branch "$b" "$SRC" "$W/view/$b"
  echo "files of branch $b are in: $W/view/$b"
  (cd "$W/view/$b" && find . -path ./.git -prune -o -type f -print | sort)
  ;;
reset)
  if [ "$MODE" = local ]; then
    # .ovc-dev/github belongs to github mode: keep it
    find "$W" -mindepth 1 -maxdepth 1 ! -name github -exec rm -rf {} + 2>/dev/null || true
  else
    rm -rf "$W"
  fi
  echo "removed $W (mode=$MODE; Oracle container untouched; the remote itself is NOT touched)"
  ;;
*)
  sed -n '2,20p' "$0"
  exit 2
  ;;
esac
