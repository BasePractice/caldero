CREATE TABLE wallet
(
    id         UUID      NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    user_id    UUID               DEFAULT NULL,
    type       VARCHAR   NOT NULL DEFAULT 'USER' CHECK ( type IN ('USER', 'COMMON') ),
    state      VARCHAR   NOT NULL DEFAULT 'ACTIVE' CHECK ( state IN ('ACTIVE', 'BLOCKED', 'DELETED', 'CLOSED') ),
    balance    BIGINT    NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    UNIQUE (user_id, type)
);

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

CREATE OR REPLACE FUNCTION fn_update_after_transaction() RETURNS TRIGGER AS
$$
BEGIN
    IF NEW.operation = 'DEBIT' AND NEW.state = 'SUCCESS' THEN
        UPDATE wallet SET balance = balance + NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    END IF;
    IF NEW.operation = 'CREDIT' AND NEW.state = 'SUCCESS' THEN
        UPDATE wallet SET balance = balance - NEW.value, updated_at = current_timestamp WHERE id = NEW.target;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE 'plpgsql';

CREATE TRIGGER transaction_update_after
    AFTER UPDATE
    ON transaction
    FOR EACH ROW
    WHEN ( NEW.state != OLD.state AND NEW.state = 'SUCCESS')
EXECUTE FUNCTION fn_update_after_transaction();


