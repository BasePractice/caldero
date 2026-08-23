-- fosite сравнивает секрет клиента как bcrypt-хеш (DefaultClient.GetHashedSecret
-- проходит через хешер), поэтому открытый текст не совпадал никогда:
-- аутентификация клиента возвращала invalid_client при верном секрете.
--
-- Значение ниже — bcrypt от 'test-secret'. Это отладочный клиент для
-- локального стенда; секреты реальных клиентов в репозитории не хранятся.
UPDATE oauth_clients
SET client_secret = '$2a$10$j8qPY3J9n2AqPfeeKiV1V.yq92EBdg105TfaYME9DWn17TYxZGHkq'
WHERE client_id = 'test-client'
  AND client_secret = 'test-secret';
