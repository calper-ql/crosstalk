#!/usr/bin/env bash
# cli.sh — build crosstalk and run the server in the foreground.
#
# First run generates ./run/crosstalkd.toml with a fresh token; later runs
# reuse it, so the token and the TLS cert (and every client's pin) survive
# restarts. Ctrl-C stops the server.
#
#   ./cli.sh                               # bind 127.0.0.1:27677 (this machine only)
#   CROSSTALK_BIND=0.0.0.0:27677 ./cli.sh  # expose to the whole subnet (deliberate)
#
# The default is loopback-only: exposing the server fleet-wide is an explicit
# opt-in, matching the design's "no default bind" principle. An existing
# run/crosstalkd.toml is reused as-is, so this changes only fresh setups.
set -euo pipefail
cd "$(dirname "$0")"

BIND="${CROSSTALK_BIND:-127.0.0.1:27677}"
CONFIG=run/crosstalkd.toml

# Build identity: git describe stamps every binary, so `crosstalk version`
# names the exact build and `crosstalk update` pulls whenever the server's
# build differs — not only on a version-number bump.
STAMP=$(git describe --always --dirty 2>/dev/null || echo untracked)
LDFLAGS="-s -w -X github.com/calper-ql/crosstalk/internal/proto.Build=$STAMP"

echo "==> cross-compiling client matrix (build $STAMP, embedded into the server)"
ASSETS=internal/dist/assets
mkdir -p bin run "$ASSETS"
for target in linux/amd64 linux/arm64 darwin/arm64; do
    os=${target%/*} arch=${target#*/}
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags "$LDFLAGS" -o "$ASSETS/crosstalk-$os-$arch" ./cmd/crosstalk
done
(cd "$ASSETS" && sha256sum crosstalk-* > SHA256SUMS)

echo "==> building server + local client (build $STAMP)"
go build -trimpath -ldflags "$LDFLAGS" -o bin ./cmd/...

if [[ ! -f $CONFIG ]]; then
    TOKEN=$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
    umask 077
    cat > "$CONFIG" <<EOF
bind  = "$BIND"
token = "$TOKEN"
EOF
    echo "==> wrote $CONFIG (token generated, mode 0600)"
elif [[ -n ${CROSSTALK_BIND:-} ]]; then
    # Config exists; a bind override applies to it in place.
    sed -i "s|^bind .*|bind  = \"$BIND\"|" "$CONFIG"
fi

echo "local client built at: $PWD/bin/crosstalk"

# crosstalkd prints the copyable curl install line at startup.
exec ./bin/crosstalkd -config "$CONFIG"
