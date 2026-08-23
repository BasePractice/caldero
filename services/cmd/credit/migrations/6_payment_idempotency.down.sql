DROP INDEX IF EXISTS payment_idempotency_key_uniq;
ALTER TABLE payment
    DROP COLUMN IF EXISTS idempotency_key;
