-- Погашение кредита списывает средства с кошелька — это межсервисная
-- денежная операция. Ключ идемпотентности хранится рядом с платежом,
-- чтобы повтор запроса не списал средства второй раз.
ALTER TABLE payment
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR;
CREATE UNIQUE INDEX IF NOT EXISTS payment_idempotency_key_uniq
    ON payment (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- Фактически внесённая сумма может быть нулевой только у запланированного
-- платежа; фактическое время появляется вместе с оплатой.
ALTER TABLE payment
    ALTER COLUMN amount SET DEFAULT 0;
