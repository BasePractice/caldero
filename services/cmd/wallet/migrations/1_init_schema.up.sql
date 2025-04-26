CREATE TABLE wallet
(
    id      UUID    NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id UUID             DEFAULT NULL,
    type    VARCHAR NOT NULL DEFAULT 'USER' CHECK ( type IN ('USER', 'COMMON') ),
    UNIQUE (user_id, type)
);

CREATE TABLE transaction
(
    id         UUID      NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    type       VARCHAR   NOT NULL DEFAULT 'PLAIN' CHECK ( type IN ('PLAIN') ),
    source     UUID      NOT NULL,
    target     UUID               DEFAULT NULL,
    state      VARCHAR   NOT NULL DEFAULT 'CREATE' CHECK ( state IN ('CREATE', 'SUCCESS', 'FAILURE', 'REJECTED') ),
    message    VARCHAR            DEFAULT NULL,
    details    JSONB              DEFAULT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    FOREIGN KEY (source) REFERENCES wallet (id)
);

