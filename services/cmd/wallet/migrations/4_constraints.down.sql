CREATE OR REPLACE FUNCTION fn_update_after_transaction() RETURNS TRIGGER AS
$$
BEGIN
    IF NEW.operation = 'DEBIT' AND NEW.state = 'SUCCESS' THEN
        UPDATE wallet SET balance = balance + NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    END IF;
    IF NEW.operation = 'CREDIT' AND NEW.state = 'SUCCESS' THEN
        UPDATE wallet SET balance = balance - NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE 'plpgsql';

ALTER TABLE transaction
    ALTER COLUMN created_at TYPE TIMESTAMP,
    ALTER COLUMN updated_at TYPE TIMESTAMP;
ALTER TABLE wallet
    ALTER COLUMN created_at TYPE TIMESTAMP,
    ALTER COLUMN updated_at TYPE TIMESTAMP;

ALTER TABLE wallet
    DROP CONSTRAINT IF EXISTS wallet_balance_not_negative;
ALTER TABLE transaction
    DROP CONSTRAINT IF EXISTS transaction_value_positive;
