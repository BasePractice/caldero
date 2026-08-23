-- Публичный клиент веб-интерфейса. Секрета у него нет и быть не может:
-- браузерное приложение не хранит секретов, поэтому подлинность обмена
-- обеспечивает PKCE.
--
-- Адрес возврата — страница самого интерфейса. На локальном стенде это
-- порт 3000; для другого стенда клиент заводится своей записью, а не
-- правкой этой.
INSERT INTO oauth_clients (client_id, client_secret, redirect_uris,
                           grant_types, response_types, scopes)
VALUES ('web', '', 'http://localhost:3000/',
        'authorization_code,refresh_token', 'code', 'openid,read,write')
ON CONFLICT (client_id) DO NOTHING;
