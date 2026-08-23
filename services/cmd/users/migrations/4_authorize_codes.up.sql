-- Коды авторизации и запросы PKCE хранятся в той же таблице, что и токены:
-- у них тот же набор полей, и переиспользование избавляет от второй копии
-- кода сериализации сессии.
ALTER TABLE oauth_tokens
    DROP CONSTRAINT IF EXISTS oauth_tokens_token_type_check;
ALTER TABLE oauth_tokens
    ADD CONSTRAINT oauth_tokens_token_type_check
        CHECK (token_type IN ('access', 'refresh', 'code', 'pkce'));

-- Код авторизации одноразовый. Использованный код не удаляется, а помечается:
-- повторное предъявление должно отличаться от «кода не существует», иначе
-- перехваченный код нельзя отличить от мусора и нечем обнаружить атаку.
ALTER TABLE oauth_tokens
    ADD COLUMN IF NOT EXISTS used BOOLEAN NOT NULL DEFAULT FALSE;

-- Запись PKCE хранится под той же подписью, что и код авторизации, поэтому
-- первичный ключ только по signature приводил к нарушению уникальности
-- в середине потока авторизации.
ALTER TABLE oauth_tokens
    DROP CONSTRAINT IF EXISTS oauth_tokens_pkey;
ALTER TABLE oauth_tokens
    ADD CONSTRAINT oauth_tokens_pkey PRIMARY KEY (signature, token_type);

CREATE INDEX IF NOT EXISTS oauth_tokens_request_id_idx ON oauth_tokens (request_id);
CREATE INDEX IF NOT EXISTS oauth_tokens_expires_at_idx ON oauth_tokens (expires_at);
