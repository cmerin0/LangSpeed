-- 0002: admin/moderator accounts (Requirement 2.4).
-- Columns: id, username, hashed_password, role, created_at, updated_at.
-- The role column accepts only 'admin' and 'moderator' and rejects anything
-- else (Requirement 2.7).
CREATE TABLE users (
    id              bigserial PRIMARY KEY,
    username        varchar(64) NOT NULL UNIQUE,
    hashed_password text        NOT NULL,
    role            varchar(16) NOT NULL CHECK (role IN ('admin', 'moderator')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
