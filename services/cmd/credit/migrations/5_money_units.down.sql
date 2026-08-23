DROP TABLE IF EXISTS credit_archive;

ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_already_paid_valid;
ALTER TABLE credit
    RENAME COLUMN last_paid_at TO last_payed_at;
ALTER TABLE credit
    ALTER COLUMN already_paid TYPE INTEGER;
ALTER TABLE credit
    RENAME COLUMN already_paid TO already_payed;
ALTER TABLE credit
    ADD CONSTRAINT credit_already_payed_not_negative CHECK (already_payed >= 0);

ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_rate_in_range;
UPDATE credit
SET rate_bp = rate_bp / 100;
ALTER TABLE credit
    ALTER COLUMN rate_bp SET DEFAULT 10;
ALTER TABLE credit
    RENAME COLUMN rate_bp TO percent;
ALTER TABLE credit
    ADD CONSTRAINT credit_percent_not_negative CHECK (percent >= 0);

CREATE TABLE credit_archive AS
SELECT *
FROM credit WITH NO DATA;
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_only_completed_state CHECK (state IN ('REJECTED', 'COMPLETED'));
