-- payment ссылается на credit внешним ключом, поэтому удаляется первой.
DROP TABLE IF EXISTS payment;
DROP TABLE IF EXISTS credit_archive;
DROP TABLE IF EXISTS credit;
