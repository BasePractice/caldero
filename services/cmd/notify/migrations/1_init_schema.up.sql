CREATE TABLE event
(
    id         UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    UUID        NOT NULL,
    type       VARCHAR     NOT NULL,
    payload    JSONB       NOT NULL DEFAULT '{}'::JSONB,
    dedup_key  VARCHAR              DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- Повтор публикации не должен превращаться во второе сообщение: публикующий
-- сервис ретраит запрос, не зная, дошёл ли предыдущий.
CREATE UNIQUE INDEX event_dedup_idx ON event (user_id, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX event_user_created_idx ON event (user_id, created_at DESC);

CREATE TABLE delivery
(
    id              UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    event_id        UUID        NOT NULL REFERENCES event (id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL,
    channel         VARCHAR     NOT NULL CHECK ( channel IN ('IN_APP', 'TELEGRAM') ),
    state           VARCHAR     NOT NULL DEFAULT 'PENDING' CHECK ( state IN ('PENDING', 'DELIVERED', 'FAILED') ),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    last_error      VARCHAR              DEFAULT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    -- Одно событие — одна доставка в канал. Повторный запуск публикации
    -- не должен породить вторую отправку в тот же Telegram.
    UNIQUE (event_id, channel)
);

-- Очередь выбирается по состоянию и времени следующей попытки.
CREATE INDEX delivery_pending_idx ON delivery (next_attempt_at) WHERE state = 'PENDING';
-- Ограничение частоты считает отправленное пользователю за окно,
-- то есть по времени отправки, а не постановки в очередь.
CREATE INDEX delivery_user_channel_idx ON delivery (user_id, channel, updated_at DESC);

CREATE TABLE message
(
    id         UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    UUID        NOT NULL,
    seq        BIGINT      NOT NULL,
    event_id   UUID        NOT NULL REFERENCES event (id) ON DELETE CASCADE,
    type       VARCHAR     NOT NULL,
    title      VARCHAR     NOT NULL,
    body       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    read_at    TIMESTAMPTZ          DEFAULT NULL,
    UNIQUE (user_id, seq),
    -- Одно событие — одно сообщение в ленте: повторная доставка после
    -- сбоя не должна показать пользователю дубль.
    UNIQUE (event_id)
);

-- Номер сообщения выдаётся отдельной строкой на пользователя, а не общей
-- последовательностью: значения BIGSERIAL становятся видимыми в порядке
-- фиксации транзакций, а не в порядке выдачи, и длинный опрос по курсору
-- пропустил бы сообщение, зафиксированное позже соседа с большим номером.
CREATE TABLE message_sequence
(
    user_id  UUID   NOT NULL PRIMARY KEY,
    last_seq BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE preference
(
    user_id    UUID        NOT NULL,
    type       VARCHAR     NOT NULL,
    channel    VARCHAR     NOT NULL CHECK ( channel IN ('IN_APP', 'TELEGRAM') ),
    enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (user_id, type, channel)
);

CREATE TABLE telegram_binding
(
    user_id         UUID        NOT NULL PRIMARY KEY,
    chat_id         BIGINT               DEFAULT NULL,
    -- Хранится хеш кода привязки, а не сам код: база с кодами — это
    -- список готовых способов привязать чужой аккаунт к своему боту.
    code_hash       BYTEA                DEFAULT NULL,
    code_expires_at TIMESTAMPTZ          DEFAULT NULL,
    bound_at        TIMESTAMPTZ          DEFAULT NULL,
    -- blocked выставляется, когда бот заблокирован пользователем: слать
    -- в такой чат бессмысленно, а провайдер считает это ошибкой.
    blocked    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    UNIQUE (chat_id)
);

CREATE UNIQUE INDEX telegram_binding_code_idx ON telegram_binding (code_hash) WHERE code_hash IS NOT NULL;
