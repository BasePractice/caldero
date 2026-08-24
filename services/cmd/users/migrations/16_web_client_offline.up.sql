-- Область offline_access для веб-интерфейса.
--
-- Без неё провайдер не выдаёт refresh_token (fosite считает областями
-- обновления offline и offline_access), и сессия интерфейса кончается
-- вместе с часом жизни токена доступа — прямо посреди работы.
--
-- Сам refresh_token живёт там же, где и токен доступа: в памяти вкладки.
-- Перезагрузка страницы по-прежнему требует входа заново.
UPDATE oauth_clients
SET scopes = scopes || ',offline_access'
WHERE client_id = 'web'
  AND scopes NOT LIKE '%offline_access%';
