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
) PARTITION BY RANGE (created_at);

CREATE TABLE transaction_2025_04 PARTITION OF transaction FOR VALUES FROM ('2025-04-01') TO ('2025-05-01');
CREATE TABLE transaction_2025_05 PARTITION OF transaction FOR VALUES FROM ('2025-05-01') TO ('2025-06-01');

CREATE OR REPLACE FUNCTION create_next_partition() RETURNS VOID AS
$$
DECLARE
    next_month TEXT := to_char(now() + INTERVAL '1 month', 'YYYY_MM');
    start_date DATE := date_trunc('month', now() - INTERVAL '1 month');
    end_date   DATE := start_date + INTERVAL '1 month';
BEGIN
    EXECUTE format(
            'CREATE TABLE transaction_%s PARTITION OF transaction FOR VALUES FROM (%L) TO (%L)',
            next_month,
            start_date,
            end_date
            );
END;
$$ LANGUAGE plpgsql;

CREATE EXTENSION pg_cron;
SELECT cron.schedule(
               'create-next-partition',
               '0 0 26 * *',
               $$SELECT create_next_partition()$$
       );

