DROP TRIGGER IF EXISTS transaction_update_after ON transaction;
DROP FUNCTION IF EXISTS fn_update_after_transaction();

-- transaction ссылается на wallet внешними ключами, поэтому удаляется первой:
-- обратный порядок приводил к ошибке и делал откат невозможным.
DROP TABLE IF EXISTS transaction;
DROP TABLE IF EXISTS wallet;
