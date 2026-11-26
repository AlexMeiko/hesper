#!/usr/bin/env bash
set -euo pipefail

OUT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/test/.tls-certs}"
mkdir -p "$OUT_DIR"

# Generate ephemeral test PKI for Docker MySQL 8.4 / MariaDB 11.4 / PostgreSQL 16 TLS + mTLS tests.
# Never commit files generated under test/.tls-certs/.

# 1. Primary Test Root CA
openssl req -x509 -newkey rsa:2048 -nodes -days 3 \
  -keyout "$OUT_DIR/ca.key" \
  -out "$OUT_DIR/ca.crt" \
  -subj "/CN=Hesper Test Root CA" >/dev/null 2>&1

# 2. Secondary Untrusted Root CA (for negative verify-ca / client-cert tests)
openssl req -x509 -newkey rsa:2048 -nodes -days 3 \
  -keyout "$OUT_DIR/other-ca.key" \
  -out "$OUT_DIR/other-ca.crt" \
  -subj "/CN=Hesper Untrusted CA" >/dev/null 2>&1

# 3. Server certificate (SAN covers 127.0.0.1, localhost, db.hesper.test; deliberately omits *.internal aliases)
cat > "$OUT_DIR/server.ext" <<'EOF'
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = IP:127.0.0.1, DNS:localhost, DNS:db.hesper.test
EOF

openssl req -newkey rsa:2048 -nodes \
  -keyout "$OUT_DIR/server.key" \
  -out "$OUT_DIR/server.csr" \
  -subj "/CN=127.0.0.1" >/dev/null 2>&1

openssl x509 -req -in "$OUT_DIR/server.csr" \
  -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
  -out "$OUT_DIR/server.crt" -days 3 \
  -extfile "$OUT_DIR/server.ext" >/dev/null 2>&1

# 4. Trusted mTLS client certificate (CN=sqlm_mtls matches PG cert auth role and MySQL/MariaDB REQUIRE X509 user)
cat > "$OUT_DIR/client.ext" <<'EOF'
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth
EOF

openssl req -newkey rsa:2048 -nodes \
  -keyout "$OUT_DIR/client.key" \
  -out "$OUT_DIR/client.csr" \
  -subj "/CN=sqlm_mtls" >/dev/null 2>&1

openssl x509 -req -in "$OUT_DIR/client.csr" \
  -CA "$OUT_DIR/ca.crt" -CAkey "$OUT_DIR/ca.key" -CAcreateserial \
  -out "$OUT_DIR/client.crt" -days 3 \
  -extfile "$OUT_DIR/client.ext" >/dev/null 2>&1

# 5. Untrusted client certificate signed by other-ca (for negative mTLS tests)
openssl req -newkey rsa:2048 -nodes \
  -keyout "$OUT_DIR/wrong-client.key" \
  -out "$OUT_DIR/wrong-client.csr" \
  -subj "/CN=sqlm_mtls" >/dev/null 2>&1

openssl x509 -req -in "$OUT_DIR/wrong-client.csr" \
  -CA "$OUT_DIR/other-ca.crt" -CAkey "$OUT_DIR/other-ca.key" -CAcreateserial \
  -out "$OUT_DIR/wrong-client.crt" -days 3 \
  -extfile "$OUT_DIR/client.ext" >/dev/null 2>&1

rm -f "$OUT_DIR"/*.csr "$OUT_DIR"/*.ext "$OUT_DIR"/*.srl

# Containers run DB daemons as non-root UIDs (mysql/postgres); PostgreSQL init copies server.key to $PGDATA with 0600.
chmod 644 "$OUT_DIR"/*.crt "$OUT_DIR"/*.key

echo "Generated ephemeral TLS test PKI in $OUT_DIR"
