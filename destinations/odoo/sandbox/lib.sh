# Shared helpers for destinations/odoo/sandbox/*. Source only.
set -euo pipefail

SANDBOX="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SANDBOX/../../.." && pwd)"
# shellcheck source=../../../versions.env
source "$ROOT/versions.env"

ODOO_IMAGE="odoo:18.0@sha256:9668abacd894276ed009e8db7db1d1ffb8068af6982f76a3400ed0d68e40432d" # Odoo 18.0-20261015
ODOO_DB_NAME="${ODOO_DB_NAME:-smr1}"
ODOO_PORT="${STEELMATTER_ODOO_PORT:-18069}"
ANCHOR_PORT="${STEELMATTER_ODOO_ANCHOR_PORT:-55442}"
STEELMATTER_DESTINATION="${STEELMATTER_DESTINATION:-acme-in}"
SECRETS="$ROOT/.local/odoo-sandbox.env"
ANCHOR_ENV="$ROOT/.local/odoo-anchor.env"
export POSTGRES_IMAGE ODOO_IMAGE ODOO_DB_NAME ODOO_PORT ANCHOR_PORT STEELMATTER_DESTINATION

log() { printf '==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

compose() { docker compose -f "$SANDBOX/compose.yaml" "$@"; }

load_secrets() {
  [[ -f "$SECRETS" ]] || die "$SECRETS missing; run destinations/odoo/sandbox/up"
  set -a; source "$SECRETS"; set +a
  if [[ -f "$ANCHOR_ENV" ]]; then
    set -a; source "$ANCHOR_ENV"; set +a
    # The Odoo container reaches the anchor by its service name.
    ANCHOR_DEST_URL_IN_NETWORK="${STEELMATTER_ANCHOR_DEST_URL/127.0.0.1:$ANCHOR_PORT/anchor-db:5432}"
  else
    ANCHOR_DEST_URL_IN_NETWORK=""
  fi
  export ANCHOR_DEST_URL_IN_NETWORK
}
