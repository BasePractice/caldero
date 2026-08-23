-- Профиль пользователя. Телефон по требованию обязателен, но объявить его
-- NOT NULL одним шагом нельзя: у существующих пользователей его нет.
-- Колонка добавляется nullable, обязательность проверяется в коде при
-- регистрации, а ужесточение схемы — отдельным шагом после дозаполнения.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS phone VARCHAR,
    ADD COLUMN IF NOT EXISTS phone_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS email VARCHAR,
    ADD COLUMN IF NOT EXISTS gender VARCHAR,
    ADD COLUMN IF NOT EXISTS display_name VARCHAR,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Телефон и почта уникальны: по ним пользователя находят.
CREATE UNIQUE INDEX IF NOT EXISTS users_phone_uniq ON users (phone) WHERE phone IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_uniq ON users (lower(email)) WHERE email IS NOT NULL;

ALTER TABLE users
    ADD CONSTRAINT users_gender_check CHECK (gender IS NULL OR gender IN ('MALE', 'FEMALE', 'OTHER'));
-- Формат E.164: плюс и от 8 до 15 цифр.
ALTER TABLE users
    ADD CONSTRAINT users_phone_format CHECK (phone IS NULL OR phone ~ '^\+[1-9][0-9]{7,14}$');
