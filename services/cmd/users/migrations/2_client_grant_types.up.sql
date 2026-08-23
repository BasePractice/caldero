-- Authorization Code Flow не реализован (хранилище кодов отсутствует),
-- поэтому клиент не должен его заявлять: иначе fosite примет запрос
-- и упрётся в неподдерживаемую операцию уже внутри потока.
UPDATE oauth_clients
SET grant_types = 'password,refresh_token'
WHERE grant_types LIKE '%authorization_code%';
