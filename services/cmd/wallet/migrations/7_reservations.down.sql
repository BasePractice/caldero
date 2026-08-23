DROP TRIGGER IF EXISTS transaction_update_after ON transaction;

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

CREATE TRIGGER transaction_update_after
    AFTER UPDATE
    ON transaction
    FOR EACH ROW
    WHEN ( NEW.state != OLD.state AND NEW.state = 'SUCCESS')
EXECUTE FUNCTION fn_update_after_transaction();

DROP INDEX IF EXISTS transaction_reserved_until_idx;
ALTER TABLE transaction
    DROP COLUMN IF EXISTS reserved_until;
