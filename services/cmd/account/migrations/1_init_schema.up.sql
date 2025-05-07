CREATE TABLE account
(
    id         SERIAL    NOT NULL PRIMARY KEY,
    user_id    UUID      NOT NULL,
    type       VARCHAR   NOT NULL DEFAULT 'DEBIT' CHECK ( type IN ('DEBIT', 'CREDIT') ),
    credit_id  UUID               DEFAULT NULL CHECK ( (credit_id IS NOT NULL AND type = 'CREDIT') OR
                                                       (credit_id IS NULL AND type = 'DEBIT') ),
    state      VARCHAR   NOT NULL DEFAULT 'ACTIVE' CHECK ( state IN ('ACTIVE', 'BLOCKED')),
    balance    BIGINT    NOT NULL DEFAULT 0,
    started_at TIMESTAMP          DEFAULT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp
);
