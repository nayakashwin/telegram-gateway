#!/bin/sh
# Generates a self-signed TLS certificate for the compose Postgres instance.
# Run once: ./scripts/gen-db-certs.sh
# Outputs to ./db-certs/ (gitignored).
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)/db-certs"
mkdir -p "$DIR"

# CN=db matches the compose service hostname the gateway connects to.
openssl req -new -x509 -days 3650 -nodes \
  -subj "/CN=db" \
  -keyout "$DIR/server.key" \
  -out "$DIR/server.crt" 2>/dev/null

# Postgres requires the key to be owned by postgres with mode 0600.
chmod 600 "$DIR/server.key"

echo "Certificates written to $DIR/"
echo "Set DB_TLS_CERTS_DIR=$DIR (or run from repo root; compose mounts ./db-certs by default)."
