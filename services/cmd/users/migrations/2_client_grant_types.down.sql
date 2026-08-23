UPDATE oauth_clients
SET grant_types = 'authorization_code,refresh_token,password'
WHERE grant_types = 'password,refresh_token';
