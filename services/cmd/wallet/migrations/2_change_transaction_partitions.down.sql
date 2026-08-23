ALTER TABLE transaction
    RENAME TO transaction_partitioned;

CREATE TABLE transaction
(
    id         UUID      NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    type       VARCHAR   NOT NULL DEFAULT 'PLAIN' CHECK ( type IN ('PLAIN') ),
    target     UUID      NOT NULL,
    source     UUID               DEFAULT NULL,
    state      VARCHAR   NOT NULL DEFAULT 'RESERVED' CHECK ( state IN ('RESERVED', 'SUCCESS', 'FAILURE', 'REJECTED') ),
    operation  VARCHAR   NOT NULL CHECK ( operation IN ('DEBIT', 'CREDIT', 'SWAP') ),
    value      BIGINT    NOT NULL DEFAULT 0,
    message    VARCHAR            DEFAULT NULL,
    details    JSONB              DEFAULT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    FOREIGN KEY (source) REFERENCES wallet (id),
    FOREIGN KEY (target) REFERENCES wallet (id)
);

INSERT INTO transaction (id, type, target, source, state, operation, value, message, details, created_at, updated_at)
SELECT id, type, target, source, state, operation, value, message, details, created_at, updated_at
FROM transaction_partitioned;

DROP TABLE transaction_partitioned;

CREATE TRIGGER transaction_update_after
    AFTER UPDATE
    ON transaction
    FOR EACH ROW
    WHEN ( NEW.state != OLD.state AND NEW.state = 'SUCCESS')
EXECUTE FUNCTION fn_update_after_transaction();
