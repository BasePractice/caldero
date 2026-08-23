ALTER TABLE account
    ALTER COLUMN started_at TYPE TIMESTAMP,
    ALTER COLUMN created_at TYPE TIMESTAMP,
    ALTER COLUMN updated_at TYPE TIMESTAMP;
ALTER TABLE account
    DROP CONSTRAINT IF EXISTS account_balance_not_negative;
