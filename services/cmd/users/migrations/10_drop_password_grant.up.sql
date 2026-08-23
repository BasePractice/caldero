-- Грант password исключён из OAuth 2.1 и помечен устаревшим в fosite:
-- клиент получает пароль пользователя в открытом виде, что несовместимо
-- с внешними провайдерами входа и вторым фактором.
-- Его заменяет Authorization Code Flow с PKCE.
UPDATE oauth_clients
SET grant_types = 'authorization_code,refresh_token'
WHERE grant_types LIKE '%password%';

-- Заодно нормализуется redirect_uri: значение из первой миграции содержало
-- порт 0001, и сравнение с http://localhost:1/callback давало invalid_request.
UPDATE oauth_clients
SET redirect_uris = 'http://localhost:1/callback'
WHERE client_id = 'test-client'
  AND redirect_uris = 'http://localhost:0001/callback';
