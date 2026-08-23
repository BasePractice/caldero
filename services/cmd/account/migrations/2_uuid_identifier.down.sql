ALTER TABLE account
    ADD COLUMN serial_id SERIAL;
ALTER TABLE account
    DROP CONSTRAINT account_pkey;
ALTER TABLE account
    DROP COLUMN id;
ALTER TABLE account
    RENAME COLUMN serial_id TO id;
ALTER TABLE account
    ADD CONSTRAINT account_pkey PRIMARY KEY (id);
