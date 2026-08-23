UPDATE oauth_clients
SET grant_types = 'authorization_code,password,refresh_token'
WHERE client_id = 'test-client';

UPDATE oauth_clients
SET redirect_uris = 'http://localhost:0001/callback'
WHERE client_id = 'test-client';
