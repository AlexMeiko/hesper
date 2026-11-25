-- Real MySQL 8.4 + InnoDB test & demo fixture for sql-manager
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
    (3, '陈知夏', '', NULL, 'free', 0, '2026-03-08 18:00:05'),
    (4, 'Alex Meiko', 'alex@example.com', '关注跨平台桌面与数据库工具', 'enterprise', 1, '2026-05-12 11:45:10'),
    (5, '苏晚晴', 'su@example.com', '   ', 'pro', 1, '2026-06-01 08:00:00');

-- Precision & special types fidelity table: BIGINT, UNSIGNED BIGINT, DECIMAL(38,10), NULL vs '', JSON, BLOB
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
    (18446744073709551615, 9007199254740993, -9223372036854775808, 1234567890123456789012345678.1234567890, NULL, '{"kind":"boundary","safe_js_max":false}', UNHEX('89504E470D0A1A0A0000000D49484452'), '2026-11-25 01:02:03.123456'),
    (18446744073709551614, 9223372036854775807, -9007199254740993, -0.0000000001, '', '{"note":"empty_string_not_null","tags":["mysql","8.4"]}', UNHEX('DEADBEEFCAFEBABE'), '2026-11-25 02:03:04.000001'),
    (1, 42, 0, 100.5000000000, 'normal text 中文 🚀', NULL, NULL, '2026-11-25 03:04:05.000000');

-- Multi-page pagination table (1,250 rows)
CREATE TABLE events (
    id BIGINT NOT NULL AUTO_INCREMENT,
    category VARCHAR(40) NOT NULL,
    severity TINYINT NOT NULL,
    message VARCHAR(255) NULL,
    payload JSON NULL,
    created_at DATETIME NOT NULL,
    PRIMARY KEY (id),
    KEY idx_events_category (category)
) ENGINE=InnoDB;

DELIMITER //
CREATE PROCEDURE seed_events()
BEGIN
    DECLARE i INT DEFAULT 1;
    WHILE i <= 1250 DO
        INSERT INTO events (category, severity, message, payload, created_at)
        VALUES (
            ELT((i MOD 4) + 1, 'auth', 'query', 'billing', 'sync'),
            (i MOD 5) + 1,
            CASE WHEN i MOD 10 = 0 THEN NULL WHEN i MOD 10 = 5 THEN '' ELSE CONCAT('Event #', i, ' - 业务日志记录') END,
            JSON_OBJECT('seq', i, 'ok', (i MOD 7) != 0),
            TIMESTAMPADD(SECOND, i, '2026-11-01 00:00:00')
        );
        SET i = i + 1;
    END WHILE;
END //
DELIMITER ;

CALL seed_events();
DROP PROCEDURE seed_events;

-- Edge tables for TableEditability inspection
CREATE TABLE legacy_logs (
    id BIGINT NOT NULL AUTO_INCREMENT,
    body VARCHAR(200) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=MyISAM;
INSERT INTO legacy_logs (body) VALUES ('myisam non-transactional sample');

CREATE TABLE no_pk_notes (
    title VARCHAR(100) NOT NULL,
    content TEXT NULL
) ENGINE=InnoDB;
INSERT INTO no_pk_notes (title, content) VALUES ('note-1', 'no primary key table');

CREATE VIEW v_customer_summary AS
SELECT id, name, tier, active FROM customers;

-- Independent writable test database for Execute-Once & CommitOutcomeUnknown tests
USE sqlm_write_test;

CREATE TABLE write_probe (
    id BIGINT NOT NULL AUTO_INCREMENT,
    tag VARCHAR(128) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ENGINE=InnoDB;

CREATE TABLE side_effect_log (
    id BIGINT NOT NULL AUTO_INCREMENT,
    run_key VARCHAR(128) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (id)
) ENGINE=InnoDB;

DELIMITER //
CREATE PROCEDURE sp_side_effect_rows(IN p_key VARCHAR(128), IN p_rows INT)
BEGIN
    INSERT INTO side_effect_log (run_key) VALUES (p_key);
    WITH RECURSIVE seq(n) AS (
        SELECT 1
        UNION ALL
        SELECT n + 1 FROM seq WHERE n < p_rows
    )
    SELECT n AS row_num, p_key AS run_key, CONCAT('item-', n) AS label FROM seq;
END //
DELIMITER ;
