-- Enable mTLS verification user for MariaDB 11.4 TLS/mTLS integration acceptance tests.
CREATE USER IF NOT EXISTS 'sqlm_mtls'@'%' IDENTIFIED BY 'sqlm_mtls_pw' REQUIRE X509;
GRANT SELECT ON sqlm_demo.* TO 'sqlm_mtls'@'%';
FLUSH PRIVILEGES;
