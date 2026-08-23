-- Запрос информации о кошельках агрегирует транзакции по target,
-- без индекса это полное сканирование transaction на каждый вызов.
CREATE INDEX IF NOT EXISTS transaction_target_idx ON transaction (target);
CREATE INDEX IF NOT EXISTS transaction_source_idx ON transaction (source) WHERE source IS NOT NULL;

-- Отбор резервов идёт по паре состояние-операция.
CREATE INDEX IF NOT EXISTS transaction_state_operation_idx ON transaction (state, operation);

-- История операций и будущее партиционирование по месяцу (T-025).
CREATE INDEX IF NOT EXISTS transaction_created_at_idx ON transaction (created_at);

-- Кошельки выбираются по владельцу.
CREATE INDEX IF NOT EXISTS wallet_user_id_idx ON wallet (user_id);
