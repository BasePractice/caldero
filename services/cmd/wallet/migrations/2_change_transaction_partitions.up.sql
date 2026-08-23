-- Требование №6 из README кошелька: партиционирование транзакций по месяцу
-- создания. Раньше этот файл состоял из закомментированного черновика
-- и занимал номер версии, создавая видимость выполненной работы.
--
-- Ключ партиционирования обязан входить в первичный ключ, поэтому он
-- становится составным: (id, created_at).
ALTER TABLE transaction
    RENAME TO transaction_unpartitioned;

CREATE TABLE transaction
(
    id         UUID      NOT NULL DEFAULT gen_random_uuid(),
    type       VARCHAR   NOT NULL DEFAULT 'PLAIN' CHECK ( type IN ('PLAIN') ),
    target     UUID      NOT NULL,
    source     UUID               DEFAULT NULL,
    state      VARCHAR   NOT NULL DEFAULT 'RESERVED' CHECK ( state IN ('RESERVED', 'SUCCESS', 'FAILURE', 'REJECTED') ),
    operation  VARCHAR   NOT NULL CHECK ( operation IN ('DEBIT', 'CREDIT', 'SWAP') ),
    value      BIGINT    NOT NULL DEFAULT 0,
    message    VARCHAR            DEFAULT NULL,
    details    JSONB              DEFAULT NULL,
    -- TIMESTAMPTZ задаётся сразу: после того как created_at станет ключом
    -- партиционирования, PostgreSQL запрещает менять тип колонки.
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (id, created_at),
    FOREIGN KEY (source) REFERENCES wallet (id),
    FOREIGN KEY (target) REFERENCES wallet (id)
) PARTITION BY RANGE (created_at);

-- Партиции на два года вперёд плюс на год назад под уже накопленные данные.
DO
$$
    DECLARE
        month_start DATE := date_trunc('month', now() - INTERVAL '12 months');
        month_end   DATE;
    BEGIN
        FOR i IN 0..35
            LOOP
                month_end := month_start + INTERVAL '1 month';
                EXECUTE format(
                        'CREATE TABLE IF NOT EXISTS transaction_%s PARTITION OF transaction FOR VALUES FROM (%L) TO (%L)',
                        to_char(month_start, 'YYYY_MM'), month_start, month_end);
                month_start := month_end;
            END LOOP;
    END
$$;

-- Страховка: без неё вставка за пределами созданного окна падает целиком.
-- Автоматическое создание партиций вперёд — отдельная задача.
CREATE TABLE IF NOT EXISTS transaction_default PARTITION OF transaction DEFAULT;

-- Значения без часового пояса трактуются как локальное время сервера.
INSERT INTO transaction (id, type, target, source, state, operation, value, message, details, created_at, updated_at)
SELECT id, type, target, source, state, operation, value, message, details, created_at, updated_at
FROM transaction_unpartitioned;

DROP TABLE transaction_unpartitioned;

CREATE TRIGGER transaction_update_after
    AFTER UPDATE
    ON transaction
    FOR EACH ROW
    WHEN ( NEW.state != OLD.state AND NEW.state = 'SUCCESS')
EXECUTE FUNCTION fn_update_after_transaction();
