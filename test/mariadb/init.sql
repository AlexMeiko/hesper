-- Real MariaDB 11.4 test & verification fixture for sql-manager (Phase 1C)
SET NAMES utf8mb4;

CREATE DATABASE IF NOT EXISTS sqlm_demo CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE DATABASE IF NOT EXISTS sqlm_write_test CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- 1. True read-only database user (SELECT + SHOW VIEW on sqlm_demo only)
CREATE USER IF NOT EXISTS 'sqlm_ro'@'%' IDENTIFIED BY 'sqlm_ro_pw';
GRANT SELECT, SHOW VIEW ON sqlm_demo.* TO 'sqlm_ro'@'%';

-- 2. Writable user for demo and independent write test DB
CREATE USER IF NOT EXISTS 'sqlm_rw'@'%' IDENTIFIED BY 'sqlm_rw_pw';
GRANT ALL PRIVILEGES ON sqlm_demo.* TO 'sqlm_rw'@'%';
GRANT ALL PRIVILEGES ON sqlm_write_test.* TO 'sqlm_rw'@'%';
FLUSH PRIVILEGES;

USE sqlm_demo;

CREATE TABLE customers (
    id BIGINT NOT NULL AUTO_INCREMENT,
    name VARCHAR(120) NOT NULL,
    email VARCHAR(160) NULL,
    bio TEXT NULL,
    tier ENUM('free', 'pro', 'enterprise') NOT NULL DEFAULT 'free',
    active TINYINT(1) NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id),
    UNIQUE KEY uk_customers_email (email)
) ENGINE=InnoDB;

INSERT INTO customers (id, name, email, bio, tier, active, created_at) VALUES
    (1, '林清源', 'lin@example.com', '核心架构师', 'enterprise', 1, '2026-01-15 09:30:00'),
    (2, '周明远', NULL, '', 'pro', 1, '2026-02-20 14:15:22'),
    (3, '陈知夏', '', '   ', 'free', 0, '2026-03-08 18:00:05');

-- Fidelity table: BIGINT UNSIGNED, BIGINT, DECIMAL(38,10), NULL vs '', JSON, BLOB, UTF8MB4
CREATE TABLE ledger_entries (
    id BIGINT UNSIGNED NOT NULL,
    snowflake_id BIGINT NOT NULL,
    signed_min BIGINT NOT NULL,
    amount DECIMAL(38, 10) NOT NULL,
    note VARCHAR(255) NULL,
    metadata JSON NULL,
    raw_bytes VARBINARY(128) NULL,
    recorded_at DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB;

INSERT INTO ledger_entries VALUES
    (18446744073709551615, 9007199254740993, -9223372036854775808, 1234567890123456789012345678.1234567890, NULL, '{"engine":"mariadb","boundary":true}', UNHEX('89504E470D0A1A0A0000000D49484452'), '2026-11-25 01:02:03.123456'),
    (18446744073709551614, 9223372036854775807, -9007199254740993, -0.0000000001, '', '{"empty":true}', UNHEX('DEADBEEFCAFEBABE'), '2026-11-25 02:03:04.000001'),
    (1, 42, 0, 100.5000000000, 'MariaDB 中文 🚀', NULL, NULL, '2026-11-25 03:04:05.000000');

-- Multi-page pagination table (250 rows)
CREATE TABLE events (
    id BIGINT NOT NULL AUTO_INCREMENT,
    category VARCHAR(40) NOT NULL,
    message VARCHAR(255) NULL,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB;

INSERT INTO events (category, message, created_at)
SELECT
    ELT((seq MOD 4) + 1, 'auth', 'query', 'billing', 'sync'),
    CASE WHEN seq MOD 10 = 0 THEN NULL WHEN seq MOD 10 = 5 THEN '' ELSE CONCAT('MariaEvent #', seq) END,
    TIMESTAMPADD(SECOND, seq, '2026-11-01 00:00:00')
FROM seq_1_to_250;

-- MariaDB-specific SEQUENCE object (not present in MySQL!)
CREATE SEQUENCE order_seq START WITH 1000 INCREMENT BY 1;
CREATE SEQUENCE invoice_seq START WITH 5000 INCREMENT BY 5;

-- Non-transactional storage engine & keyless table for TableEditability inspection
CREATE TABLE legacy_logs (
    id BIGINT NOT NULL AUTO_INCREMENT,
    body VARCHAR(200) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=MyISAM;
INSERT INTO legacy_logs (body) VALUES ('myisam sample in mariadb');

CREATE TABLE no_pk_notes (
    title VARCHAR(100) NOT NULL,
    content TEXT NULL
) ENGINE=InnoDB;
INSERT INTO no_pk_notes (title, content) VALUES ('note-1', 'no pk');

CREATE VIEW v_customer_summary AS
SELECT id, name, tier, active FROM customers;

-- Independent writable DB for INSERT ... RETURNING Execute-Once & CommitOutcomeUnknown tests
USE sqlm_write_test;

CREATE TABLE write_probe (
    id BIGINT NOT NULL AUTO_INCREMENT,
    tag VARCHAR(128) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ENGINE=InnoDB;
