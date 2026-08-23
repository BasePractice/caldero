DROP TRIGGER IF EXISTS wallet_state_transition ON wallet;
DROP FUNCTION IF EXISTS fn_wallet_state_transition();
DROP INDEX IF EXISTS transaction_idempotency_key_uniq;
ALTER TABLE transaction
    DROP COLUMN IF EXISTS idempotency_key;
