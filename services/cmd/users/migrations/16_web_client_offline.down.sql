UPDATE oauth_clients
SET scopes = replace(scopes, ',offline_access', '')
WHERE client_id = 'web';
