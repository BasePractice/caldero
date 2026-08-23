ALTER TABLE credit_archive
    DROP COLUMN id;
ALTER TABLE credit_archive
    ADD COLUMN id INTEGER;

ALTER TABLE credit
    ADD COLUMN serial_id SERIAL;
ALTER TABLE payment
    ADD COLUMN credit_serial BIGINT;

UPDATE payment p
SET credit_serial = c.serial_id
FROM credit c
WHERE p.credit_id = c.id;

ALTER TABLE payment
    DROP CONSTRAINT IF EXISTS payment_credit_id_fkey;
ALTER TABLE payment
    DROP COLUMN credit_id;
ALTER TABLE payment
    RENAME COLUMN credit_serial TO credit_id;

ALTER TABLE credit
    DROP CONSTRAINT credit_pkey;
ALTER TABLE credit
    DROP COLUMN id;
ALTER TABLE credit
    RENAME COLUMN serial_id TO id;
ALTER TABLE credit
    ADD CONSTRAINT credit_pkey PRIMARY KEY (id);

ALTER TABLE payment
    ALTER COLUMN credit_id SET NOT NULL;
ALTER TABLE payment
    ADD CONSTRAINT payment_credit_id_fkey FOREIGN KEY (credit_id) REFERENCES credit (id) ON DELETE NO ACTION;
