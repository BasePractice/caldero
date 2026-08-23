-- Каналов стало четыре: к ленте приложения и Telegram добавляются
-- мессенджер МАКС и электронная почта.
ALTER TABLE delivery
    DROP CONSTRAINT delivery_channel_check;
ALTER TABLE delivery
    ADD CONSTRAINT delivery_channel_check
        CHECK ( channel IN ('IN_APP', 'TELEGRAM', 'MAX', 'EMAIL') );

ALTER TABLE preference
    DROP CONSTRAINT preference_channel_check;
ALTER TABLE preference
    ADD CONSTRAINT preference_channel_check
        CHECK ( channel IN ('IN_APP', 'TELEGRAM', 'MAX', 'EMAIL') );

-- Привязка мессенджера перестаёт быть привязкой одного Telegram:
-- механизм у ботов одинаковый, и вторая его реализация разошлась бы
-- с первой при первой же правке.
ALTER TABLE telegram_binding
    RENAME TO messenger_binding;

ALTER TABLE messenger_binding
    ADD COLUMN provider VARCHAR NOT NULL DEFAULT 'TELEGRAM'
        CHECK ( provider IN ('TELEGRAM', 'MAX') );

-- Ключ теперь составной: у пользователя может быть привязан и Telegram,
-- и МАКС одновременно.
ALTER TABLE messenger_binding
    DROP CONSTRAINT telegram_binding_pkey;
ALTER TABLE messenger_binding
    ADD CONSTRAINT messenger_binding_pkey PRIMARY KEY (provider, user_id);

ALTER TABLE messenger_binding
    DROP CONSTRAINT telegram_binding_chat_id_key;
ALTER TABLE messenger_binding
    ADD CONSTRAINT messenger_binding_chat_uniq UNIQUE (provider, chat_id);

DROP INDEX IF EXISTS telegram_binding_code_idx;
CREATE UNIQUE INDEX messenger_binding_code_idx
    ON messenger_binding (provider, code_hash) WHERE code_hash IS NOT NULL;
