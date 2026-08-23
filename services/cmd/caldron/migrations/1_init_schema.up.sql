CREATE TABLE caldron
(
    id                   UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    creator_id           UUID        NOT NULL,
    title                VARCHAR     NOT NULL,
    type                 VARCHAR     NOT NULL CHECK ( type IN ('GIFT', 'LUCK') ),
    state                VARCHAR     NOT NULL DEFAULT 'PREPARING'
        CHECK ( state IN ('PREPARING', 'READY', 'SETTLED', 'CANCELLED') ),
    -- Два вида котла из README: создатель либо скидывается вместе со всеми,
    -- либо остаётся арбитром.
    creator_participates BOOLEAN     NOT NULL DEFAULT TRUE,
    mode                 VARCHAR     NOT NULL CHECK ( mode IN ('FIXED', 'INDIVIDUAL', 'RANGE') ),
    amount               BIGINT               DEFAULT NULL,
    min_amount           BIGINT               DEFAULT NULL,
    max_amount           BIGINT               DEFAULT NULL,
    -- Кошелёк котла: средства участников лежат на нём, а не на кошельке
    -- создателя, иначе сбор смешивается с его собственными деньгами.
    wallet_id            UUID                 DEFAULT NULL,
    settled_at           TIMESTAMPTZ          DEFAULT NULL,
    cancelled_at         TIMESTAMPTZ          DEFAULT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    -- У каждого режима свой набор сумм. Проверка в схеме, а не только
    -- в коде: запись мимо сервиса не должна оставить котёл без правила,
    -- по которому считается взнос.
    -- Сравнения обёрнуты в IS NOT NULL намеренно: NULL > 0 даёт не false,
    -- а NULL, и такой CHECK пропускает запись вместо того, чтобы отклонить.
    CONSTRAINT caldron_mode_amounts CHECK (
        (mode = 'FIXED' AND amount IS NOT NULL AND amount > 0
            AND min_amount IS NULL AND max_amount IS NULL)
            OR (mode = 'INDIVIDUAL' AND amount IS NULL AND min_amount IS NULL AND max_amount IS NULL)
            OR (mode = 'RANGE' AND amount IS NULL
            AND min_amount IS NOT NULL AND min_amount > 0
            AND max_amount IS NOT NULL AND max_amount >= min_amount)
        )
);

CREATE TABLE participant
(
    caldron_id  UUID        NOT NULL REFERENCES caldron (id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL,
    -- Ожидаемая сумма: у FIXED берётся из котла, у INDIVIDUAL назначается
    -- создателем, у RANGE не задана заранее.
    expected    BIGINT               DEFAULT NULL CHECK ( expected IS NULL OR expected > 0 ),
    contributed BIGINT      NOT NULL DEFAULT 0 CHECK ( contributed >= 0 ),
    state       VARCHAR     NOT NULL DEFAULT 'INVITED'
        CHECK ( state IN ('INVITED', 'PAID', 'REFUNDED') ),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    PRIMARY KEY (caldron_id, user_id),
    -- Внесённое есть ровно у того, кто внёс: без этого «внёс» и «сколько»
    -- разъезжаются, а по ним считается сумма котла.
    CONSTRAINT participant_paid_amount CHECK (
        (state = 'INVITED' AND contributed = 0) OR (state <> 'INVITED' AND contributed > 0)
        )
);

-- Котлы читают по создателю и по участнику.
CREATE INDEX caldron_creator_idx ON caldron (creator_id);
CREATE INDEX participant_user_idx ON participant (user_id);
-- Возвраты добивает фоновая задача: ищет отменённые котлы, где кто-то
-- ещё числится внёсшим.
CREATE INDEX participant_pending_refund_idx ON participant (caldron_id) WHERE state = 'PAID';
