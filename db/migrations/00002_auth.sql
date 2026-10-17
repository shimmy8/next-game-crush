-- +goose Up
CREATE TABLE openid_login_states (
    token_hash bytea PRIMARY KEY,
    return_path text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE openid_response_nonces (
    nonce_hash bytea PRIMARY KEY,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS openid_response_nonces, openid_login_states;
