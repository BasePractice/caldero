-- Ставка переводится из целых процентов в базисные пункты: проценты
-- не выражают даже 12,5 %, а дробные типы для денег не годятся.
-- Колонка переименована намеренно: значение 2400 в колонке с прежним
-- именем percent читалось бы как 2400 % и молча ломало расчёт.
ALTER TABLE credit
    RENAME COLUMN percent TO rate_bp;
UPDATE credit
SET rate_bp = rate_bp * 100;
ALTER TABLE credit
    ALTER COLUMN rate_bp SET DEFAULT 1000;
ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_percent_not_negative;
ALTER TABLE credit
    ADD CONSTRAINT credit_rate_in_range CHECK (rate_bp >= 100 AND rate_bp <= 30000);

-- already_payed переименована к остальным полям и расширена до BIGINT:
-- INTEGER ограничивал сумму примерно двадцатью одним миллионом копеек.
ALTER TABLE credit
    RENAME COLUMN already_payed TO already_paid;
ALTER TABLE credit
    ALTER COLUMN already_paid TYPE BIGINT;
ALTER TABLE credit
    RENAME COLUMN last_payed_at TO last_paid_at;

ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_already_payed_not_negative;
ALTER TABLE credit
    ADD CONSTRAINT credit_already_paid_valid CHECK (already_paid >= 0 AND already_paid < balance);

DROP TABLE IF EXISTS credit_archive;
CREATE TABLE credit_archive
(
    LIKE credit INCLUDING DEFAULTS INCLUDING CONSTRAINTS
);
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_pkey PRIMARY KEY (id);
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_only_completed_state CHECK (state IN ('REJECTED', 'COMPLETED'));
