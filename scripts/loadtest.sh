#!/usr/bin/env bash
#
# Local load test: start the built binary with two real blocklists, fire every
# domain on them at it, and print the resulting metrics.
#
# Every query is expected to come back NXDOMAIN, so this is a correctness check
# as much as a throughput one — a domain that resolves means it was parsed into
# a form that cannot match.
#
#   ./scripts/loadtest.sh                 # ~576k domains, all of them
#   ./scripts/loadtest.sh -n 20000        # a quick subset
#   ./scripts/loadtest.sh -w 64           # more concurrency
#
set -euo pipefail

LISTS=(
  "https://media.githubusercontent.com/media/zachlagden/Pi-hole-Optimized-Blocklists/main/lists/comprehensive.txt"
  "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"
)

WORKERS=32
LIMIT=0
DNS_PORT=1053
METRICS_PORT=9253
READY_PORT=8281
CACHE_DIR="${TMPDIR:-/tmp}/homedns-loadtest"

usage() {
  sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
  exit "${1:-0}"
}

while getopts "n:w:p:c:h" opt; do
  case "$opt" in
    n) LIMIT=$OPTARG ;;
    w) WORKERS=$OPTARG ;;
    p) DNS_PORT=$OPTARG ;;
    c) CACHE_DIR=$OPTARG ;;
    h) usage ;;
    *) usage 1 ;;
  esac
done

cd "$(dirname "$0")/.."
mkdir -p "$CACHE_DIR"

WORK=$(mktemp -d)
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null && wait "$SERVER_PID" 2>/dev/null
  rm -rf "$WORK"
  return 0
}
trap cleanup EXIT

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# --- build -------------------------------------------------------------------
say "Building"
go build -o "$WORK/homedns" .
go build -o "$WORK/loadgen" ./scripts/loadgen
echo "  CoreDNS $(go list -m -f '{{.Version}}' github.com/coredns/coredns)"

# --- query list --------------------------------------------------------------
# Downloaded separately from the server's own fetch: the server exercises the
# real code path, this is just to know what to ask it about. Cached, because
# these are ~18 MB.
say "Fetching lists"
for url in "${LISTS[@]}"; do
  dest="$CACHE_DIR/$(printf '%s' "$url" | sha256sum | cut -c1-16).txt"
  if [ -s "$dest" ]; then
    echo "  cached  $(basename "$url") ($(wc -l < "$dest") lines)"
  else
    echo "  GET     $url"
    curl -fsSL --max-time 300 -o "$dest" "$url"
    echo "          $(wc -l < "$dest") lines"
  fi
done

QUERIES="$WORK/queries.txt"
# Mirrors plugin/blocklist/parse.go, so the query set is exactly what the server
# loaded. It has to include the localNames rejection: an /etc/hosts preamble
# maps localhost and friends, the plugin deliberately does not block them, and
# asking about them would report a failure that isn't one.
cat "$CACHE_DIR"/*.txt \
  | sed 's/#.*//' \
  | awk '
      BEGIN {
        split("localhost localhost.localdomain local broadcasthost " \
              "ip6-localhost ip6-loopback ip6-localnet ip6-mcastprefix " \
              "ip6-allnodes ip6-allrouters ip6-allhosts", l, " ")
        for (i in l) skip[l[i]] = 1
      }
      {
        if (NF >= 2 && $1 ~ /^([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+|[0-9a-fA-F:]*:[0-9a-fA-F:]*)/) name = $2
        else if (NF == 1) name = $1
        else next
        sub(/^\*\./, "", name); sub(/^\./, "", name); sub(/\.$/, "", name)
        name = tolower(name)
        if (name in skip) next
        if (name ~ /\./ && name !~ /^[0-9.]+$/ && name !~ /[^a-z0-9._-]/) print name
      }' \
  | sort -u > "$QUERIES"

TOTAL=$(wc -l < "$QUERIES")
echo "  $TOTAL unique blockable domains"

# --- server ------------------------------------------------------------------
cat > "$WORK/Corefile" <<EOF
.:${DNS_PORT} {
    blocklist {
$(printf '        url %s\n' "${LISTS[@]}")
        bootstrap_dns 1.1.1.1 9.9.9.9
        ready_timeout 600s
    }
    hosts {
        10.99.99.1 control.loadtest.invalid
    }
    prometheus 127.0.0.1:${METRICS_PORT}
    ready 127.0.0.1:${READY_PORT}
    errors
}
EOF

say "Starting server"
"$WORK/homedns" -conf "$WORK/Corefile" > "$WORK/server.log" 2>&1 &
SERVER_PID=$!

# ready_timeout is long on purpose: /ready must mean "a list is loaded", not
# "gave up and failed open".
started=$(date +%s)
until [ "$(curl -s -o /dev/null -w '%{http_code}' "127.0.0.1:${READY_PORT}/ready" || true)" = "200" ]; do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    echo "server exited:"; cat "$WORK/server.log"; exit 1
  fi
  if [ $(( $(date +%s) - started )) -gt 600 ]; then
    echo "timed out waiting for /ready"; cat "$WORK/server.log"; exit 1
  fi
  sleep 0.5
done
echo "  ready after $(( $(date +%s) - started ))s"

metric() { curl -s "127.0.0.1:${METRICS_PORT}/metrics" | awk -v k="$1" '$1==k {print $2}'; }

if [ "$(metric coredns_blocklist_fail_open)" != "0" ]; then
  echo "  blocklist failed open — lists did not load, aborting"; cat "$WORK/server.log"; exit 1
fi
echo "  loaded $(metric coredns_blocklist_domains_total | cut -d. -f1) domain subtrees"
echo "  RSS    $(awk '/VmRSS/{print $2" "$3}' "/proc/$SERVER_PID/status")"

# --- fire --------------------------------------------------------------------
say "Blocked domains (expect NXDOMAIN)"
BLOCKED_OK=0
"$WORK/loadgen" -server "127.0.0.1:${DNS_PORT}" -file "$QUERIES" \
  -workers "$WORKERS" -limit "$LIMIT" -expect NXDOMAIN && BLOCKED_OK=1 || true

say "Control names (expect NOERROR)"
printf 'control.loadtest.invalid\n%.0s' {1..2000} > "$WORK/control.txt"
"$WORK/loadgen" -server "127.0.0.1:${DNS_PORT}" -file "$WORK/control.txt" \
  -workers "$WORKERS" -expect NOERROR || true

echo
echo "  RSS after load: $(awk '/VmRSS/{print $2" "$3}' "/proc/$SERVER_PID/status")"

# --- metrics -----------------------------------------------------------------
say "Metrics (127.0.0.1:${METRICS_PORT}/metrics)"
curl -s "127.0.0.1:${METRICS_PORT}/metrics" \
  | grep -E '^(homedns_|coredns_blocklist_|coredns_dns_requests_total|coredns_dns_responses_total|coredns_dns_request_duration_seconds_(sum|count)|coredns_panics_total)' \
  | sed 's/^/  /'

say "Server log"
sed 's/^/  /' "$WORK/server.log"

if [ "$BLOCKED_OK" != "1" ]; then
  echo
  echo "FAILED: not every blocked domain answered NXDOMAIN."
  exit 1
fi
