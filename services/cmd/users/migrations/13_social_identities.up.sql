-- Внешние идентичности: один локальный пользователь может входить
-- несколькими способами.
CREATE TABLE identity
(
    provider    VARCHAR     NOT NULL,
    -- Идентификатор у провайдера. Именно он связывает вход с пользователем:
    -- почта у провайдера может меняться и повторяться, а этот — нет.
    external_id VARCHAR     NOT NULL,
    user_id     UUID        NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    -- Почта сохраняется как справочная: подтверждением она не считается.
    email       VARCHAR              DEFAULT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,

    PRIMARY KEY (provider, external_id)
);

CREATE INDEX identity_user_idx ON identity (user_id);

-- Начатый вход через внешнего провайдера. Хранится в базе, а не в памяти:
-- ответ провайдера может прийти на другой экземпляр сервиса.
CREATE TABLE social_login
(
    -- state защищает от подделки ответа: без сверки злоумышленник
    -- подсовывает свой код авторизации и связывает свой внешний аккаунт
    -- с чужой сессией.
    state           VARCHAR     NOT NULL PRIMARY KEY,
    provider        VARCHAR     NOT NULL,
    -- Проверочный код PKCE: перехваченный код авторизации без него
    -- обменивается на токен кем угодно.
    verifier        VARCHAR     NOT NULL,
    -- Исходный запрос авторизации: после внешнего входа продолжается
    -- ровно тот поток, который начал клиент.
    authorize_query TEXT        NOT NULL DEFAULT '',
    -- Заполнено, если вход начат уже вошедшим пользователем ради привязки.
    link_user_id    UUID                 DEFAULT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

-- У пользователя, созданного через внешнего провайдера, пароля нет.
-- Без этого признака нельзя отличить «пароль не задан» от «задан»,
-- а значит и запретить отвязку последнего способа входа.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS password_set BOOLEAN NOT NULL DEFAULT TRUE;
