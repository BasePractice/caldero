-- Партиции старше срока хранения отсоединяются, а не удаляются.
--
-- DETACH оставляет таблицу на месте: она перестаёт быть частью transaction,
-- запросы истории её больше не видят, но данные никуда не деваются
-- и выгружаются обычным pg_dump. Удаление финансовой истории — операция,
-- которую нельзя отменить, и делать её по расписанию нельзя: сервис
-- отсоединяет, а удаляет человек, осознанно и отдельными правами.
--
-- Блокировка на время отсоединения берётся на transaction, поэтому
-- обслуживание идёт редко и вне пиков. CONCURRENTLY здесь не берётся:
-- прерванная команда оставляет партицию в промежуточном состоянии,
-- которое нужно доводить вручную, а выигрыш на месячном обслуживании
-- нулевой.
CREATE OR REPLACE FUNCTION fn_detach_transaction_partitions(keep_months INT) RETURNS INT AS
$$
DECLARE
    -- Граница считается от начала текущего месяца: партиция месячная,
    -- и отсоединять её можно только целиком.
    cutoff    DATE := (date_trunc('month', now()) - (keep_months || ' months')::INTERVAL)::DATE;
    partition RECORD;
    detached  INT  := 0;
BEGIN
    IF keep_months <= 0 THEN
        RETURN 0;
    END IF;

    FOR partition IN
        SELECT child.relname AS name
        FROM pg_inherits
                 JOIN pg_class child ON child.oid = pg_inherits.inhrelid
        WHERE pg_inherits.inhparent = 'transaction'::REGCLASS
          -- Партиция по умолчанию под шаблон не подходит и не отсоединяется
          -- никогда: в ней лежит то, что не попало ни в один месяц.
          AND child.relname ~ '^transaction_[0-9]{4}_[0-9]{2}$'
          AND to_date(right(child.relname, 7), 'YYYY_MM') < cutoff
        ORDER BY child.relname
        LOOP
            EXECUTE format('ALTER TABLE transaction DETACH PARTITION %I', partition.name);
            detached := detached + 1;
        END LOOP;
    RETURN detached;
END;
$$ LANGUAGE plpgsql;

-- Возраст самой старой присоединённой партиции: по нему видно, работает ли
-- обслуживание вообще. Без метрики отсоединение либо идёт, либо молча
-- не идёт, и узнать об этом можно только по размеру таблицы.
CREATE OR REPLACE FUNCTION fn_oldest_transaction_partition() RETURNS DATE AS
$$
SELECT min(to_date(right(child.relname, 7), 'YYYY_MM'))
FROM pg_inherits
         JOIN pg_class child ON child.oid = pg_inherits.inhrelid
WHERE pg_inherits.inhparent = 'transaction'::REGCLASS
  AND child.relname ~ '^transaction_[0-9]{4}_[0-9]{2}$';
$$ LANGUAGE sql STABLE;
