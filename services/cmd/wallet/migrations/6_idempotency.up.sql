-- Ключ идемпотентности: повтор запроса при обрыве связи не должен провести
-- операцию дважды. Для денежных операций это не удобство, а требование —
-- клиент не может отличить «запрос не дошёл» от «ответ не дошёл».
ALTER TABLE transaction
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR;

-- Уникальность на партиционированной таблице обязана включать ключ
-- партиционирования, поэтому она составная. Этого достаточно: повторы
-- приходят в пределах одного месяца, а поиск идёт по индексу ниже.
CREATE UNIQUE INDEX IF NOT EXISTS transaction_idempotency_key_uniq
    ON transaction (idempotency_key, created_at)
    WHERE idempotency_key IS NOT NULL;

-- Смена состояния кошелька: DELETED терминально, из остальных состояний
-- можно вернуться в ACTIVE.
CREATE OR REPLACE FUNCTION fn_wallet_state_transition() RETURNS TRIGGER AS
$$
BEGIN
    IF OLD.state = NEW.state THEN
        RETURN NEW;
    END IF;
    IF OLD.state = 'DELETED' THEN
        RAISE EXCEPTION 'wallet % is deleted, state can not be changed', OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_state_transition
    BEFORE UPDATE OF state
    ON wallet
    FOR EACH ROW
EXECUTE FUNCTION fn_wallet_state_transition();
