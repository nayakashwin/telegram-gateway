#!/bin/sh
# Generates a CA and a server certificate for the compose Postgres instance.
# Run once: ./scripts/gen-db-certs.sh
# Outputs to ./db-certs/ (gitignored):
#   ca.crt        - CA certificate (pinned by the gateway via sslrootcert)
#   server.crt    - Postgres server certificate signed by the CA
#   server.key    - Postgres server private key
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)/db-certs"
mkdir -p "$DIR"

# 1. CA key + certificate.
openssl req -new -x509 -days 3650 -nodes \
  -subj "/CN=telegram-gateway-ca" \
  -keyout "$DIR/ca.key" \
  -out "$DIR/ca.crt" 2>/dev/null

# 2. Server key + CSR. CN=db matches the compose service hostname the gateway
#    connects to; the SAN ensures modern TLS verification succeeds.
openssl req -new -nodes \
  -subj "/CN=db" \
  -keyout "$DIR/server.key" \
  -out "$DIR/server.csr" 2>/dev/null

cat > "$DIR/server.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:db,IP:127.0.0.1
EOF

# 3. Sign the server cert with the CA.
openssl x509 -req -days 3650 \
  -in "$DIR/server.csr" \
  -CA "$DIR/ca.crt" \
  -CAkey "$DIR/ca.key" \
  -CAcreateserial \
  -extfile "$DIR/server.ext" \
  -out "$DIR/server.crt" 2>/dev/null

# Postgres requires the key to be owned by postgres with mode 0600.
chmod 600 "$DIR/server.key"

rm -f "$DIR/server.csr" "$DIR/server.ext" "$DIR/ca.srl"

echo "Certificates written to $DIR/"
echo "  ca.crt       -> pin in the gateway (sslrootcert)"
echo "  server.crt   -> Postgres server cert"
echo "  server.key   -> Postgres server key"
