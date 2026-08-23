-- Срок жизни резерва: брошенный резерв не должен блокировать средства
-- навсегда. Пустое значение означает операцию без резервирования.
ALTER TABLE transaction
    ADD COLUMN IF NOT EXISTS reserved_until TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS transaction_reserved_until_idx
    ON transaction (reserved_until)
    WHERE state = 'RESERVED';

-- Прежний триггер срабатывал только на переход в SUCCESS. Теперь баланс
-- меняется и при отмене резерва, поэтому условие снято, а разбор состояний
-- перенесён внутрь функции.
DROP TRIGGER IF EXISTS transaction_update_after ON transaction;

CREATE OR REPLACE FUNCTION fn_update_after_transaction() RETURNS TRIGGER AS
$$
BEGIN
    -- Баланс меняется только при переходе в SUCCESS. Резерв на баланс
    -- не влияет: он уменьшает доступный остаток, а не сами средства.
    IF NEW.state <> 'SUCCESS' OR OLD.state = 'SUCCESS' THEN
        RETURN NEW;
    END IF;

    IF NEW.operation = 'DEBIT' THEN
        UPDATE wallet SET balance = balance + NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    ELSIF NEW.operation = 'CREDIT' THEN
        UPDATE wallet SET balance = balance - NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    ELSIF NEW.operation = 'SWAP' THEN
        IF NEW.source IS NULL THEN
            RAISE EXCEPTION 'transaction % of kind SWAP has no source wallet', NEW.id;
        END IF;
        UPDATE wallet SET balance = balance - NEW.value, updated_at = current_timestamp WHERE id = NEW.source;
        UPDATE wallet SET balance = balance + NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    ELSE
        RAISE EXCEPTION 'unsupported transaction operation %', NEW.operation;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE 'plpgsql';

CREATE TRIGGER transaction_update_after
    AFTER UPDATE OF state
    ON transaction
    FOR EACH ROW
    WHEN ( NEW.state IS DISTINCT FROM OLD.state )
EXECUTE FUNCTION fn_update_after_transaction();
