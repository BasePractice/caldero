-- Арбитр: по README создатель либо сам запускает розыгрыш, либо назначает
-- участника, который делает это за него.
ALTER TABLE caldron
    ADD COLUMN arbiter_id UUID DEFAULT NULL;

-- Обязательство и зерно розыгрыша заводятся вместе с котлом: обязательство
-- публикуется сразу, зерно раскрывается только после розыгрыша. Так участник
-- может убедиться, что исход не подбирали задним числом.
ALTER TABLE caldron
    ADD COLUMN commitment VARCHAR DEFAULT NULL,
    ADD COLUMN seed       BYTEA   DEFAULT NULL;

CREATE TABLE gift
(
    id         UUID        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    caldron_id UUID        NOT NULL REFERENCES caldron (id) ON DELETE CASCADE,
    user_id    UUID        NOT NULL,
    provider   VARCHAR     NOT NULL,
    product_id VARCHAR     NOT NULL,
    title      VARCHAR     NOT NULL,
    url        VARCHAR              DEFAULT NULL,
    -- Цена — снимок на момент добавления: на площадке она меняется,
    -- и перед розыгрышем список сверяется заново.
    price      BIGINT      NOT NULL CHECK ( price > 0 ),
    price_at   TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    -- Один и тот же товар дважды в списке — это ошибка ввода, а не выбор.
    UNIQUE (caldron_id, user_id, provider, product_id)
);

CREATE INDEX gift_caldron_user_idx ON gift (caldron_id, user_id);

CREATE TABLE draw
(
    -- Первичный ключ по котлу: розыгрыш бывает ровно один.
    caldron_id UUID        NOT NULL PRIMARY KEY REFERENCES caldron (id) ON DELETE CASCADE,
    commitment VARCHAR     NOT NULL,
    -- Зерно хранится раскрытым: после розыгрыша оно и должно быть
    -- публичным, иначе проверить результат нечем.
    seed       BYTEA       NOT NULL,
    winner_id  UUID        NOT NULL,
    gifts      JSONB       NOT NULL,
    total      BIGINT      NOT NULL CHECK ( total >= 0 ),
    payout     BIGINT      NOT NULL CHECK ( payout >= 0 ),
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- Результат розыгрыша неизменяем. Уникальности мало: она запрещает второй
-- розыгрыш, но не запрещает переписать первый. Побочное следствие принято
-- осознанно: котёл с состоявшимся розыгрышем больше не удалить — историю
-- денежной операции и не должно быть можно стереть.
CREATE OR REPLACE FUNCTION fn_draw_is_immutable() RETURNS TRIGGER AS
$$
BEGIN
    RAISE EXCEPTION 'draw result is immutable';
END;
$$ LANGUAGE 'plpgsql';

CREATE TRIGGER draw_immutable
    BEFORE UPDATE OR DELETE
    ON draw
    FOR EACH ROW
EXECUTE FUNCTION fn_draw_is_immutable();
