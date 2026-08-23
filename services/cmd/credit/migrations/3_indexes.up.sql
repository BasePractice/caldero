-- Платежи всегда выбираются по кредиту.
CREATE INDEX IF NOT EXISTS payment_credit_id_idx ON payment (credit_id);

-- Список кредитов пользователя и кредитов, выданных оператором.
CREATE INDEX IF NOT EXISTS credit_user_id_idx ON credit (user_id);
CREATE INDEX IF NOT EXISTS credit_creator_id_idx ON credit (creator_id);
