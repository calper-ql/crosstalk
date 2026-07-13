#!/usr/bin/env bash
# run.sh — build and operate a local crosstalkd.
#
# First `up` generates ./state/crosstalkd.toml with a fresh random token; later
# runs reuse it, so the token and the TLS cert (and every client's pin)
# survive restarts. Ctrl-C stops the server.
set -euo pipefail
cd "$(dirname "$0")"

CONFIG=state/crosstalkd.toml
CERT=state/cert.pem
KEY=state/key.pem
DEFAULT_BIND="127.0.0.1:27677"
MODULE=github.com/calper-ql/crosstalk

usage() {
    cat <<'EOF'
run.sh — build and operate a local crosstalkd

Usage: ./run.sh [command] [options]

Commands:
  up                 (default) build, then run the server in the foreground
  build              build binaries only, don't run
  token              print the current server token
  fingerprint        print the server's TLS certificate SHA-256 fingerprint
  reset <scope>      regenerate credentials (does NOT start the server)
  help               show this help

Options for up / build:
  --bind <ip:port>   bind address (default 127.0.0.1:27677, or $CROSSTALK_BIND)
                     loopback is the default; exposing fleet-wide is explicit:
                     --bind 0.0.0.0:27677
  --no-build         skip the (slow) cross-compile+embed step; run the
                     binaries already in ./bin

reset scopes (each breaks existing client registrations — re-register after):
  --token            new token; TLS cert/pin unchanged
  --cert             new TLS certificate; token unchanged
  --all              wipe state/ — new token AND new cert
  -y, --yes          skip the confirmation prompt

Examples:
  ./run.sh                       # build + run on 127.0.0.1:27677
  ./run.sh --bind 0.0.0.0:27677  # expose to the whole subnet
  ./run.sh up --no-build         # quick restart, no rebuild
  ./run.sh token                 # print the shared token
  ./run.sh reset --token         # rotate the token
EOF
}

gen_token() {
    openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
}

read_token() {
    [[ -f $CONFIG ]] || { echo "run.sh: no $CONFIG yet — run ./run.sh first" >&2; exit 1; }
    sed -n 's/^token *= *"\(.*\)"/\1/p' "$CONFIG"
}

build() {
    local stamp ldflags
    stamp=$(git describe --always --dirty 2>/dev/null || echo untracked)
    ldflags="-s -w -X ${MODULE}/internal/proto.Build=$stamp"
    echo "==> cross-compiling client matrix (build $stamp, embedded into the server)"
    local assets=internal/dist/assets
    mkdir -p bin state "$assets"
    local target os arch
    for target in linux/amd64 linux/arm64 darwin/arm64; do
        os=${target%/*} arch=${target#*/}
        CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
            go build -trimpath -ldflags "$ldflags" -o "$assets/crosstalk-$os-$arch" ./cmd/crosstalk
    done
    (cd "$assets" && sha256sum crosstalk-* > SHA256SUMS)
    echo "==> building server + local client (build $stamp)"
    go build -trimpath -ldflags "$ldflags" -o bin ./cmd/...
}

# ensure_config creates the config on first run (generating a token) or, when
# a bind override is given, rewrites the bind line of an existing config.
ensure_config() {
    local bind="$1"
    if [[ ! -f $CONFIG ]]; then
        local token
        token=$(gen_token)
        (umask 077; cat > "$CONFIG" <<EOF
bind  = "${bind:-$DEFAULT_BIND}"
token = "$token"
EOF
)
        echo "==> wrote $CONFIG (token generated, mode 0600)"
    elif [[ -n $bind ]]; then
        sed -i "s|^bind .*|bind  = \"$bind\"|" "$CONFIG"
        echo "==> set bind = \"$bind\" in $CONFIG"
    fi
}

confirm() {
    # $1: message. Honors $YES; prompts on an interactive terminal; refuses
    # non-interactively (so it never silently proceeds in a script).
    [[ ${YES:-0} == 1 ]] && return 0
    if [[ ! -t 0 ]]; then
        echo "run.sh: refusing without a terminal; pass --yes to confirm" >&2
        return 1
    fi
    local ans=""
    printf '%s [y/N] ' "$1"
    read -r ans || true
    [[ $ans == [yY] ]]
}

cmd_up() {
    local bind="${CROSSTALK_BIND:-}" nobuild=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --bind) bind="$2"; shift 2 ;;
            --no-build) nobuild=1; shift ;;
            *) echo "run.sh up: unknown option: $1" >&2; exit 1 ;;
        esac
    done
    [[ $nobuild == 1 ]] || build
    [[ -x bin/crosstalkd ]] || { echo "run.sh: bin/crosstalkd missing — run without --no-build" >&2; exit 1; }
    ensure_config "$bind"
    echo "local client: $PWD/bin/crosstalk  ·  token: ./run.sh token"
    # crosstalkd prints the copyable curl/register lines (with the token) at
    # startup, and runs in the foreground until Ctrl-C.
    exec ./bin/crosstalkd -config "$CONFIG"
}

cmd_reset() {
    local scope=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --token|--cert|--all) scope="$1"; shift ;;
            -y|--yes) YES=1; shift ;;
            *) echo "run.sh reset: unknown option: $1" >&2; exit 1 ;;
        esac
    done
    [[ -n $scope ]] || { echo "run.sh reset: pick a scope (--token | --cert | --all)" >&2; exit 1; }

    case "$scope" in
        --token)
            confirm "Rotate the token? Every client must re-register." || { echo "aborted"; exit 1; }
            local token; token=$(gen_token)
            if [[ -f $CONFIG ]]; then
                sed -i "s|^token *=.*|token = \"$token\"|" "$CONFIG"
            else
                ensure_config ""
                sed -i "s|^token *=.*|token = \"$token\"|" "$CONFIG"
            fi
            echo "==> new token: $token"
            echo "    re-register clients: crosstalk register <ip> 27677 hub --token $token"
            ;;
        --cert)
            confirm "Regenerate the TLS cert? Every client must re-register (pin changes)." || { echo "aborted"; exit 1; }
            rm -f "$CERT" "$KEY"
            echo "==> removed $CERT / $KEY — a fresh cert is generated on next ./run.sh"
            echo "    re-register every client afterward (the pinned fingerprint changes)."
            ;;
        --all)
            confirm "Wipe state/ (token AND cert)? Every client must re-register." || { echo "aborted"; exit 1; }
            rm -f "$CONFIG" "$CERT" "$KEY"
            echo "==> wiped $CONFIG / $CERT / $KEY — next ./run.sh starts fresh."
            ;;
    esac
}

cmd_fingerprint() {
    [[ -f $CERT ]] || { echo "run.sh: no $CERT yet — run ./run.sh first" >&2; exit 1; }
    openssl x509 -in "$CERT" -noout -fingerprint -sha256 | sed 's/.*=//'
}

# Dispatch: a leading flag (or nothing) means the default `up` command.
cmd="up"
if [[ $# -gt 0 && $1 != -* ]]; then
    cmd="$1"; shift
fi
case "$cmd" in
    up)          cmd_up "$@" ;;
    build)       build ;;
    token)       read_token ;;
    fingerprint) cmd_fingerprint ;;
    reset)       cmd_reset "$@" ;;
    help|-h|--help) usage ;;
    *) echo "run.sh: unknown command: $cmd" >&2; usage >&2; exit 1 ;;
esac
