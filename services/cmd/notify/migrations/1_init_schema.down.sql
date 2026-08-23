-- Порядок обратный созданию: таблицы со ссылками удаляются раньше тех,
-- на кого ссылаются, иначе откат падает на внешнем ключе.
DROP TABLE IF EXISTS telegram_binding;
DROP TABLE IF EXISTS preference;
DROP TABLE IF EXISTS message_sequence;
DROP TABLE IF EXISTS message;
DROP TABLE IF EXISTS delivery;
DROP TABLE IF EXISTS event;
