DELETE FROM role_permissions WHERE permission = 'platform.clients.manage';
DELETE FROM permissions WHERE node = 'platform.clients.manage';
DROP TABLE IF EXISTS oauth_grants;
DROP TABLE IF EXISTS oauth_authorization_codes;
DROP TABLE IF EXISTS oauth_clients;
