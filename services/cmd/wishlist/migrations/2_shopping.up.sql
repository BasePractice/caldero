CREATE TABLE shopping_run
(
    id         UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    UUID        NOT NULL,
    budget     BIGINT      NOT NULL CHECK ( budget > 0 ),
    -- Списано может быть меньше стоимости отобранного набора: заказать
    -- удаётся не всё, а платить за неоформленный заказ не за что.
    spent      BIGINT      NOT NULL DEFAULT 0 CHECK ( spent >= 0 ),
    -- Зерно отбора хранится, чтобы результат можно было объяснить.
    seed       BYTEA       NOT NULL,
    state      VARCHAR     NOT NULL DEFAULT 'PENDING'
        CHECK ( state IN ('PENDING', 'DONE', 'PARTIAL', 'EMPTY') ),
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    CONSTRAINT shopping_run_within_budget CHECK ( spent <= budget )
);

CREATE TABLE purchase
(
    id         UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    run_id     UUID        NOT NULL REFERENCES shopping_run (id) ON DELETE CASCADE,
    provider   VARCHAR     NOT NULL,
    product_id VARCHAR     NOT NULL,
    title      VARCHAR     NOT NULL,
    url        VARCHAR              DEFAULT NULL,
    price      BIGINT      NOT NULL CHECK ( price > 0 ),
    -- Заказ и оплата разделены: при сбое между ними нужно видеть,
    -- что именно не состоялось.
    ordered    BOOLEAN     NOT NULL DEFAULT FALSE,
    paid       BOOLEAN     NOT NULL DEFAULT FALSE,
    order_id   VARCHAR              DEFAULT NULL,
    failure    VARCHAR              DEFAULT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    -- Платить за неоформленный заказ не за что.
    CONSTRAINT purchase_paid_only_ordered CHECK ( NOT paid OR ordered )
);

CREATE INDEX shopping_run_user_idx ON shopping_run (user_id, created_at DESC);
CREATE INDEX purchase_run_idx ON purchase (run_id);
