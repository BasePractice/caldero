-- Счета выбираются по владельцу; кредитный счёт — по связанному кредиту.
CREATE INDEX IF NOT EXISTS account_user_id_idx ON account (user_id);
CREATE INDEX IF NOT EXISTS account_credit_id_idx ON account (credit_id) WHERE credit_id IS NOT NULL;
