-- Партиции создаются функцией, а не миграцией: окно, заданное при накатке,
-- рано или поздно закончится, и все транзакции пойдут в transaction_default.
-- Она не упадёт, но и смысла в партиционировании не останется.
--
-- Вызывает функцию воркер сервиса. pg_cron сюда не берётся намеренно:
-- он требует расширения в самом образе PostgreSQL, а воркер обходится тем,
-- что уже есть.
CREATE OR REPLACE FUNCTION fn_ensure_transaction_partition(month_start DATE) RETURNS BOOLEAN AS
$$
DECLARE
    partition_name TEXT := 'transaction_' || to_char(month_start, 'YYYY_MM');
    month_end      DATE := month_start + INTERVAL '1 month';
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = partition_name) THEN
        RETURN FALSE;
    END IF;

    EXECUTE format(
            'CREATE TABLE %I PARTITION OF transaction FOR VALUES FROM (%L) TO (%L)',
            partition_name, month_start, month_end);
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;
