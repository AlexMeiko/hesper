#!/usr/bin/env bash
set -euo pipefail

# Configure SSL/TLS and mTLS client-cert auth for PostgreSQL 16 test container.
if [[ ! -f /tls-certs/server.crt || ! -f /tls-certs/server.key || ! -f /tls-certs/ca.crt ]]; then
  echo "TLS certs not mounted at /tls-certs; skipping PG TLS init" >&2
  exit 1
fi

cp /tls-certs/ca.crt "$PGDATA/ca.crt"
cp /tls-certs/server.crt "$PGDATA/server.crt"
cp /tls-certs/server.key "$PGDATA/server.key"
chmod 644 "$PGDATA/ca.crt" "$PGDATA/server.crt"
chmod 600 "$PGDATA/server.key"

cat >> "$PGDATA/postgresql.conf" <<'EOF'

# Phase 2C TLS/mTLS test fixture
ssl = on
ssl_ca_file = 'ca.crt'
ssl_cert_file = 'server.crt'
ssl_key_file = 'server.key'
EOF

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "sqlm_demo" <<'SQL'
CREATE USER sqlm_mtls WITH PASSWORD 'sqlm_mtls_pw';
GRANT CONNECT ON DATABASE sqlm_demo TO sqlm_mtls;
GRANT USAGE ON SCHEMA public TO sqlm_mtls;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO sqlm_mtls;
SQL

# Prepend strict mTLS rules for sqlm_mtls ahead of default scram-sha-256 rules
# while preserving existing plaintext/TLS auth for sqlm_ro, sqlm_rw, and postgres.
tmp_hba="$(mktemp)"
cat > "$tmp_hba" <<'EOF'
# Phase 2C mTLS test rules for sqlm_mtls
hostssl sqlm_demo sqlm_mtls 0.0.0.0/0 cert clientcert=verify-full
hostssl sqlm_demo sqlm_mtls ::/0 cert clientcert=verify-full
hostnossl sqlm_demo sqlm_mtls 0.0.0.0/0 reject
hostnossl sqlm_demo sqlm_mtls ::/0 reject
EOF
cat "$PGDATA/pg_hba.conf" >> "$tmp_hba"
mv "$tmp_hba" "$PGDATA/pg_hba.conf"
chmod 600 "$PGDATA/pg_hba.conf"
