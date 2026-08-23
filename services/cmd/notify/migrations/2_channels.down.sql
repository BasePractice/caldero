DROP INDEX IF EXISTS messenger_binding_code_idx;

ALTER TABLE messenger_binding
    DROP CONSTRAINT messenger_binding_chat_uniq;
ALTER TABLE messenger_binding
    DROP CONSTRAINT messenger_binding_pkey;

-- Возврат к одному каналу: записи МАКС при откате теряются, потому что
-- в прежней схеме им негде храниться.
DELETE FROM messenger_binding WHERE provider <> 'TELEGRAM';
ALTER TABLE messenger_binding
    DROP COLUMN provider;

ALTER TABLE messenger_binding
    ADD CONSTRAINT telegram_binding_pkey PRIMARY KEY (user_id);
ALTER TABLE messenger_binding
    ADD CONSTRAINT telegram_binding_chat_id_key UNIQUE (chat_id);
ALTER TABLE messenger_binding
    RENAME TO telegram_binding;

CREATE UNIQUE INDEX telegram_binding_code_idx
    ON telegram_binding (code_hash) WHERE code_hash IS NOT NULL;

DELETE FROM delivery WHERE channel IN ('MAX', 'EMAIL');
ALTER TABLE delivery
    DROP CONSTRAINT delivery_channel_check;
ALTER TABLE delivery
    ADD CONSTRAINT delivery_channel_check CHECK ( channel IN ('IN_APP', 'TELEGRAM') );

DELETE FROM preference WHERE channel IN ('MAX', 'EMAIL');
ALTER TABLE preference
    DROP CONSTRAINT preference_channel_check;
ALTER TABLE preference
    ADD CONSTRAINT preference_channel_check CHECK ( channel IN ('IN_APP', 'TELEGRAM') );
