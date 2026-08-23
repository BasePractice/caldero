-- Та же причина, что и в credit: идентификатор счёта уходит клиенту
-- в заголовке X-Account-Id и в пути GET /account/{id}.
ALTER TABLE account
    ADD COLUMN uuid_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE account
    DROP CONSTRAINT account_pkey;
ALTER TABLE account
    DROP COLUMN id;
ALTER TABLE account
    RENAME COLUMN uuid_id TO id;
ALTER TABLE account
    ADD CONSTRAINT account_pkey PRIMARY KEY (id);
