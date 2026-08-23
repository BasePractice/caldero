-- Транзакция на отрицательную сумму инвертирует смысл операции: DEBIT
-- с value = -100 списывает средства в обход всех проверок.
ALTER TABLE transaction
    ADD CONSTRAINT transaction_value_positive CHECK (value > 0);

-- Баланс кошелька не может уйти в минус.
ALTER TABLE wallet
    ADD CONSTRAINT wallet_balance_not_negative CHECK (balance >= 0);

ALTER TABLE wallet
    ALTER COLUMN created_at TYPE TIMESTAMPTZ,
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ;
ALTER TABLE transaction
    ALTER COLUMN created_at TYPE TIMESTAMPTZ,
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ;

-- Прежний триггер молча пропускал SWAP и не трогал source, то есть перевод
-- между кошельками не менял ни одного баланса. Операция, для которой нет
-- реализации, должна отвергаться, а не выполняться наполовину.
CREATE OR REPLACE FUNCTION fn_update_after_transaction() RETURNS TRIGGER AS
$$
BEGIN
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
