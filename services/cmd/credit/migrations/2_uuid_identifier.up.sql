-- Идентификатор кредита попадает в публичный контракт (GET /credits/{id}/schedule,
-- заголовок X-Credit-Id). SERIAL предсказуем: он позволяет оценить число
-- кредитов в системе и темп их выдачи, а любая забытая проверка владельца
-- превращается из единичной утечки в массовую выгрузку перебором.
--
-- Данные переносятся, а не пересоздаются: таблицы могут быть уже наполнены.
ALTER TABLE credit
    ADD COLUMN uuid_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE payment
    ADD COLUMN credit_uuid UUID;

UPDATE payment p
SET credit_uuid = c.uuid_id
FROM credit c
WHERE p.credit_id = c.id;

ALTER TABLE payment
    DROP CONSTRAINT IF EXISTS payment_credit_id_fkey;
ALTER TABLE payment
    DROP COLUMN credit_id;
ALTER TABLE payment
    RENAME COLUMN credit_uuid TO credit_id;
ALTER TABLE payment
    ALTER COLUMN credit_id SET NOT NULL;

ALTER TABLE credit
    DROP CONSTRAINT credit_pkey;
ALTER TABLE credit
    DROP COLUMN id;
ALTER TABLE credit
    RENAME COLUMN uuid_id TO id;
ALTER TABLE credit
    ADD CONSTRAINT credit_pkey PRIMARY KEY (id);

ALTER TABLE payment
    ADD CONSTRAINT payment_credit_id_fkey FOREIGN KEY (credit_id) REFERENCES credit (id) ON DELETE NO ACTION;

-- Архив создавался через CREATE TABLE AS и унаследовал прежний тип колонки.
ALTER TABLE credit_archive
    DROP COLUMN id;
ALTER TABLE credit_archive
    ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();
