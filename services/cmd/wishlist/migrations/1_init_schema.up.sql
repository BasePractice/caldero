CREATE TABLE item
(
    id             UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id        UUID        NOT NULL,
    kind           VARCHAR     NOT NULL CHECK ( kind IN ('PRODUCT', 'MONEY') ),
    state          VARCHAR     NOT NULL DEFAULT 'VISIBLE'
        CHECK ( state IN ('VISIBLE', 'HIDDEN', 'CHOSEN', 'CONFIRMED', 'ACCEPTED', 'REJECTED') ),
    priority       INT         NOT NULL DEFAULT 3 CHECK ( priority BETWEEN 1 AND 5 ),
    title          VARCHAR     NOT NULL,
    comment        TEXT                 DEFAULT NULL,

    -- Товарные поля. Цена — снимок на момент добавления: на площадке она
    -- меняется, и показывать её как текущую нельзя.
    provider       VARCHAR              DEFAULT NULL,
    product_id     VARCHAR              DEFAULT NULL,
    url            VARCHAR              DEFAULT NULL,
    price          BIGINT               DEFAULT NULL,
    price_at       TIMESTAMPTZ          DEFAULT NULL,

    amount         BIGINT               DEFAULT NULL,

    giver_id       UUID                 DEFAULT NULL,
    reserved_until TIMESTAMPTZ          DEFAULT NULL,
    order_id       VARCHAR              DEFAULT NULL,

    created_at     TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    -- Согласовано с моделью: у товара площадка и цена, у денег сумма.
    -- Проверка в схеме, а не только в коде: запись мимо сервиса всё равно
    -- не должна оставить элемент без того, из чего он состоит.
    CONSTRAINT item_kind_fields CHECK (
        (kind = 'PRODUCT' AND provider IS NOT NULL AND product_id IS NOT NULL AND amount IS NULL)
            OR (kind = 'MONEY' AND amount IS NOT NULL AND amount > 0
            AND provider IS NULL AND product_id IS NULL)
        ),
    -- Даритель есть у всего, что кто-то выбрал, и только у него.
    CONSTRAINT item_giver_by_state CHECK (
        (state IN ('VISIBLE', 'HIDDEN') AND giver_id IS NULL)
            OR (state IN ('CHOSEN', 'CONFIRMED', 'ACCEPTED', 'REJECTED') AND giver_id IS NOT NULL)
        ),
    -- Срок резерва есть только пока подарок выбран, но не подтверждён:
    -- после подтверждения торопить дарителя нечем.
    CONSTRAINT item_reservation_by_state CHECK (
        (state = 'CHOSEN' AND reserved_until IS NOT NULL)
            OR (state <> 'CHOSEN' AND reserved_until IS NULL)
        )
);

-- Список смотрят по владельцу, сортируя по приоритету.
CREATE INDEX item_user_state_idx ON item (user_id, state, priority);
-- Просроченные резервы освобождает фоновая задача.
CREATE INDEX item_reserved_until_idx ON item (reserved_until) WHERE state = 'CHOSEN';
-- Даритель смотрит, что он уже выбрал.
CREATE INDEX item_giver_idx ON item (giver_id) WHERE giver_id IS NOT NULL;
