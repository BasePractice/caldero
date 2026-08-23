DROP INDEX IF EXISTS oauth_tokens_expires_at_idx;
DROP INDEX IF EXISTS oauth_tokens_request_id_idx;

DELETE FROM oauth_tokens WHERE token_type IN ('code', 'pkce');

ALTER TABLE oauth_tokens
    DROP CONSTRAINT IF EXISTS oauth_tokens_pkey;
ALTER TABLE oauth_tokens
    ADD CONSTRAINT oauth_tokens_pkey PRIMARY KEY (signature);

ALTER TABLE oauth_tokens
    DROP COLUMN IF EXISTS used;
ALTER TABLE oauth_tokens
    DROP CONSTRAINT IF EXISTS oauth_tokens_token_type_check;
ALTER TABLE oauth_tokens
    ADD CONSTRAINT oauth_tokens_token_type_check
        CHECK (token_type IN ('access', 'refresh'));
