ALTER TABLE users
    DROP CONSTRAINT IF EXISTS users_phone_format,
    DROP CONSTRAINT IF EXISTS users_gender_check;
DROP INDEX IF EXISTS users_email_uniq;
DROP INDEX IF EXISTS users_phone_uniq;
ALTER TABLE users
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS display_name,
    DROP COLUMN IF EXISTS gender,
    DROP COLUMN IF EXISTS email,
    DROP COLUMN IF EXISTS phone_confirmed,
    DROP COLUMN IF EXISTS phone;
