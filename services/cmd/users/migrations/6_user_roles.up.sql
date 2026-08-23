-- Роли пользователя для собственного провайдера. В режиме Keycloak роли
-- приходят из его realm, и эта таблица не используется.
CREATE TABLE IF NOT EXISTS user_roles
(
    user_id    UUID    NOT NULL REFERENCES users (user_id) ON DELETE CASCADE,
    role       VARCHAR NOT NULL CHECK (role IN ('operator', 'admin')),
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, role)
);
