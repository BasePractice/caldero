-- Схема-заглушка для проверки самого механизма миграций: применяется
-- только в тестах общего пакета services.
CREATE TABLE probe
(
    id    UUID      NOT NULL PRIMARY KEY DEFAULT gen_random_uuid(),
    note  VARCHAR   NOT NULL,
    at    TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);
