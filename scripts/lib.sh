# Shared helpers for scripts/*. Source only; not executable on its own.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
# shellcheck source=../versions.env
source "$ROOT/versions.env"
OUT="$ROOT/.out"
mkdir -p "$OUT"

# source_commit prints the source commit (T43): HEAD of this checkout, with
# -dirty when tracked files have uncommitted changes; in a release bundle,
# which has no .git, the commit scripts/release-bundle wrote to COMMIT;
# otherwise unknown. A bundle unpacked inside another repository never takes
# that repository's HEAD.
source_commit() {
  local c top
  top="$(git -C "$ROOT" rev-parse --show-toplevel 2>/dev/null || true)"
  if [[ -n "$top" && "$(cd "$top" && pwd -P)" == "$(cd "$ROOT" && pwd -P)" ]] && c="$(git -C "$ROOT" rev-parse --verify -q HEAD)"; then
    [[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]] || c="$c-dirty"
  elif [[ -f "$ROOT/COMMIT" ]] && c="$(tr -d '[:space:]' < "$ROOT/COMMIT")" && [[ "$c" =~ ^[0-9a-f]{40}$ ]]; then
    :
  else
    c=unknown
  fi
  printf '%s\n' "$c"
}

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_go() {
  command -v go >/dev/null || die "go is not installed (need $GO_VERSION)"
  local have; have="$(go env GOVERSION)"
  [[ "$have" == "go$GO_VERSION" ]] || die "go $have found; versions.env pins go$GO_VERSION"
}

# run_go_phase PHASE [go test args...]: writes .out/PHASE.json and prints a summary.
run_go_phase() {
  local phase="$1"; shift
  local file="$OUT/$phase.json" status=0
  log "phase $phase: go test $*"
  go test -json -count=1 "$@" > "$file" 2>&1 || status=$?
  go run ./cmd/smci summary "$file" || status=1
  return "$status"
}

# ensure_node_deps DIR installs a package's dependencies from its lockfile
# (lifecycle scripts disabled) when node_modules is missing or was installed
# from a different package-lock.json, so a pulled dependency change (for
# example the typescript devDependency) reaches existing checkouts.
ensure_node_deps() {
  command -v node >/dev/null || die "node is not installed (need $NODE_VERSION)"
  local dir="$1" stamp want have=""
  stamp="$dir/node_modules/.steelmatter-lock.sha256"
  want="$(node -e 'process.stdout.write(require("crypto").createHash("sha256").update(require("fs").readFileSync(process.argv[1])).digest("hex"))' "$dir/package-lock.json")"
  [[ -f "$stamp" ]] && have="$(<"$stamp")"
  if [[ ! -d "$dir/node_modules" || "$have" != "$want" ]]; then
    log "installing $(basename "$dir") dependencies from package-lock.json (scripts disabled)"
    (cd "$dir" && npm ci --ignore-scripts --no-audit --no-fund) || die "npm ci failed in $dir"
    printf '%s' "$want" > "$stamp"
  fi
}

# Integration and conformance tests run the TypeScript verifier as a
# subprocess, so they need its dependencies too, not only the verifier phase.
ensure_verifier_deps() { ensure_node_deps "$ROOT/packages/verifier"; }

# The SDK phase builds and typechecks with its pinned TypeScript.
ensure_sdk_deps() { ensure_node_deps "$ROOT/packages/sdk"; }

# The browser demo bundles the verifier source with the verifier's own pinned
# dependencies, so it needs both packages' dependencies.
ensure_demo_deps() {
  ensure_verifier_deps
  ensure_node_deps "$ROOT/demo"
}

# The MCP adapter depends on the in-repo SDK (file: link) and loads its built
# output, so the SDK is built before the adapter's dependencies are linked.
ensure_mcp_ready() {
  ensure_sdk_deps
  (cd "$ROOT/packages/sdk" && npm run --silent build)
  ensure_node_deps "$ROOT/adapters/mcp"
}
