ALTER TABLE users
    DROP COLUMN IF EXISTS password_set;
DROP TABLE IF EXISTS social_login;
DROP TABLE IF EXISTS identity;
