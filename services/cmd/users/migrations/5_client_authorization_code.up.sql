-- Authorization Code Flow реализован (хранилище кодов и PKCE), поэтому
-- отладочный клиент снова может его заявлять.
UPDATE oauth_clients
SET grant_types = 'authorization_code,password,refresh_token'
WHERE client_id = 'test-client';
