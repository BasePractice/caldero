DROP TABLE IF EXISTS confirmation;
ALTER TABLE users
    DROP COLUMN IF EXISTS email_confirmed;
