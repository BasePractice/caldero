ALTER TABLE account
    ADD CONSTRAINT account_balance_not_negative CHECK (balance >= 0);

ALTER TABLE account
    ALTER COLUMN started_at TYPE TIMESTAMPTZ,
    ALTER COLUMN created_at TYPE TIMESTAMPTZ,
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ;
