UPDATE oauth_clients
SET grant_types = 'password,refresh_token'
WHERE client_id = 'test-client';
