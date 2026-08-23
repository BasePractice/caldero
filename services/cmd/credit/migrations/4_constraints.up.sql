-- payment.id был SERIAL без PRIMARY KEY: дубликаты возможны, ссылаться
-- на строку нечем.
ALTER TABLE payment
    ADD CONSTRAINT payment_pkey PRIMARY KEY (id);

-- Суммы не могут быть отрицательными: отрицательный платёж инвертирует
-- смысл операции в обход всех проверок.
ALTER TABLE payment
    ADD CONSTRAINT payment_need_value_positive CHECK (need_value > 0);
ALTER TABLE payment
    ADD CONSTRAINT payment_amount_not_negative CHECK (amount >= 0);
ALTER TABLE credit
    ADD CONSTRAINT credit_balance_positive CHECK (balance > 0);
ALTER TABLE credit
    ADD CONSTRAINT credit_already_payed_not_negative CHECK (already_payed >= 0);
ALTER TABLE credit
    ADD CONSTRAINT credit_month_in_range CHECK (month > 0 AND month <= 600);
ALTER TABLE credit
    ADD CONSTRAINT credit_percent_not_negative CHECK (percent >= 0);

-- Ограничение UNIQUE (user_id, type) запрещало пользователю взять второй
-- кредит того же типа — это ошибка моделирования, а не бизнес-правило.
ALTER TABLE credit
    DROP CONSTRAINT IF EXISTS credit_user_id_type_key;

-- TIMESTAMP без часового пояса расходится при развёртывании в другом регионе.
ALTER TABLE credit
    ALTER COLUMN started_at TYPE TIMESTAMPTZ,
    ALTER COLUMN created_at TYPE TIMESTAMPTZ,
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ,
    ALTER COLUMN last_payed_at TYPE TIMESTAMPTZ;
ALTER TABLE payment
    ALTER COLUMN payment_at TYPE TIMESTAMPTZ,
    ALTER COLUMN expired_at TYPE TIMESTAMPTZ,
    ALTER COLUMN created_at TYPE TIMESTAMPTZ;

-- credit_archive создавался через CREATE TABLE AS и унаследовал только типы:
-- ни NOT NULL, ни значений по умолчанию, ни первичного ключа.
DROP TABLE IF EXISTS credit_archive;
CREATE TABLE credit_archive
(
    LIKE credit INCLUDING DEFAULTS INCLUDING CONSTRAINTS
);
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_pkey PRIMARY KEY (id);
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_only_completed_state CHECK (state IN ('REJECTED', 'COMPLETED'));
