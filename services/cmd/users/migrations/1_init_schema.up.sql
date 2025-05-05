CREATE TABLE IF NOT EXISTS users
(
    user_id       UUID PRIMARY KEY        DEFAULT gen_random_uuid(),
    username      VARCHAR UNIQUE NOT NULL,
    password_hash VARCHAR        NOT NULL,
    created_at    TIMESTAMP      NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS oauth_clients
(
    client_id      VARCHAR NOT NULL PRIMARY KEY,
    client_secret  VARCHAR NOT NULL,
    redirect_uris  VARCHAR NOT NULL,
    grant_types    VARCHAR NOT NULL,
    response_types VARCHAR NOT NULL,
    scopes         VARCHAR NOT NULL,
    created_at     TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
INSERT INTO oauth_clients(client_id, client_secret, redirect_uris, grant_types, response_types, scopes)
VALUES ('test-client', 'test-secret', 'http://localhost:0001/callback', 'authorization_code,refresh_token,password',
        'code', 'openid,read,write');


CREATE TABLE IF NOT EXISTS keys
(
    key_id      VARCHAR PRIMARY KEY,
    private_key BYTEA NOT NULL,
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS oauth_tokens
(
    signature    VARCHAR PRIMARY KEY,
    request_id   VARCHAR   NOT NULL,
    session_data BYTEA     NOT NULL,
    expires_at   TIMESTAMP NOT NULL,
    token_type   VARCHAR   NOT NULL CHECK (token_type IN ('access', 'refresh')),
    created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);