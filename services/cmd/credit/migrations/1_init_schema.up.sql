-- Все деньги с копейками + два порядка 00
CREATE TABLE credit
(
    id         SERIAL    NOT NULL PRIMARY KEY,
    user_id    UUID      NOT NULL,
    creator_id UUID      NOT NULL,
    type       VARCHAR   NOT NULL DEFAULT 'SIMPLE' CHECK ( type IN ('SIMPLE', 'MICRO', 'IPOT') ),
    percent    INTEGER   NOT NULL DEFAULT 10,
    state      VARCHAR   NOT NULL DEFAULT 'PREPARED' CHECK ( state IN ('PREPARED', 'CONFIRM', 'REJECTED', 'STARTED', 'COMPLETED')),
    balance    BIGINT    NOT NULL,
    started_at TIMESTAMP          DEFAULT NULL, -- время предоставления кредита
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    UNIQUE (user_id, type)
);

CREATE TABLE credit_archive AS
SELECT *
FROM credit WITH NO DATA;
ALTER TABLE credit_archive
    ADD CONSTRAINT credit_archive_only_completed_state CHECK ( state IN ('REJECTED', 'COMPLETED') );


CREATE TABLE payment
(
    id         SERIAL    NOT NULL,
    credit_id  BIGINT    NOT NULL,
    need_value BIGINT    NOT NULL,              -- необходимо внести
    amount     BIGINT    NOT NULL,              -- фактически внесено
    payment_at TIMESTAMP          DEFAULT NULL, -- фактическое время внесения средств
    state      VARCHAR   NOT NULL DEFAULT 'NEED' CHECK ( state IN ('COMPLETE', 'PARTIAL', 'NEED') ),
    expired_at TIMESTAMP NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT current_timestamp,
    FOREIGN KEY (credit_id) REFERENCES credit (id) ON DELETE NO ACTION
);
