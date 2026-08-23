-- Почта подтверждается так же, как телефон: обязательность поля — это ещё
-- не подтверждение, пользователь может ввести чужой адрес.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS email_confirmed BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE confirmation
(
    id        UUID    NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id   UUID    NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    kind      VARCHAR NOT NULL CHECK ( kind IN ('PHONE', 'EMAIL') ),
    -- Контакт на момент отправки: пользователь мог сменить номер, и код,
    -- отправленный на старый, не должен подтверждать новый.
    target    VARCHAR NOT NULL,
    -- Хранится хеш кода, а не код: таблица с кодами — это список готовых
    -- способов подтвердить чужой контакт.
    code_hash BYTEA   NOT NULL,
    -- Попытки считаются, иначе шестизначный код подбирается перебором.
    attempts     INT         NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ          DEFAULT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- Действующий код ищут по пользователю и виду; по этому же индексу
-- считается частота отправки.
CREATE INDEX confirmation_user_kind_idx ON confirmation (user_id, kind, created_at DESC);
