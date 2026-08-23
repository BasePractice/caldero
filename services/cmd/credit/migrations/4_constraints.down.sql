DROP TABLE IF EXISTS credit_archive;
CREATE TABLE credit_archive AS
SELECT *
FROM credit WITH NO DATA;
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_only_completed_state CHECK (state IN ('REJECTED', 'COMPLETED'));

ALTER TABLE payment
    ALTER COLUMN payment_at TYPE TIMESTAMP,
    ALTER COLUMN expired_at TYPE TIMESTAMP,
    ALTER COLUMN created_at TYPE TIMESTAMP;
ALTER TABLE credit
    ALTER COLUMN started_at TYPE TIMESTAMP,
    ALTER COLUMN created_at TYPE TIMESTAMP,
    ALTER COLUMN updated_at TYPE TIMESTAMP,
    ALTER COLUMN last_payed_at TYPE TIMESTAMP;

-- Восстановление UNIQUE (user_id, type) упадёт, если за время без него
-- пользователи успели взять по два кредита одного типа. Разрешать конфликт
-- автоматически нельзя: удаление кредита — это потеря финансовых данных,
-- и решение принимает человек.
ALTER TABLE credit
    ADD CONSTRAINT credit_user_id_type_key UNIQUE (user_id, type);

ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_percent_not_negative,
    DROP CONSTRAINT IF EXISTS credit_month_in_range,
    DROP CONSTRAINT IF EXISTS credit_already_payed_not_negative,
    DROP CONSTRAINT IF EXISTS credit_balance_positive;
ALTER TABLE payment
    DROP CONSTRAINT IF EXISTS payment_amount_not_negative,
    DROP CONSTRAINT IF EXISTS payment_need_value_positive,
    DROP CONSTRAINT IF EXISTS payment_pkey;
