-- +goose Up

-- LINE user IDs are the primary key: this app has no accounts of its own, and
-- a user exists here only because LINE vouched for them.
CREATE TABLE users (
    line_user_id TEXT PRIMARY KEY,
    display_name TEXT        NOT NULL DEFAULT '',
    picture_url  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE groups (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Set when the LIFF app was opened from inside a LINE chat, which lets the
    -- bot push the settlement summary back to that chat. Null for groups
    -- created from an external browser, where there is no chat to post to.
    line_group_id TEXT UNIQUE,
    name          TEXT        NOT NULL,
    created_by    TEXT        NOT NULL REFERENCES users (line_user_id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id  UUID        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id   TEXT        NOT NULL REFERENCES users (line_user_id),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

CREATE INDEX group_members_user_idx ON group_members (user_id);

CREATE TABLE bills (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id     UUID        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    payer_id     TEXT        NOT NULL REFERENCES users (line_user_id),
    title        TEXT        NOT NULL,
    -- Money is stored as integer satang. A NUMERIC would also be exact, but
    -- BIGINT keeps the arithmetic identical on both sides of the wire: the Go
    -- code does the same integer maths as Postgres, with no scale to agree on.
    total_satang BIGINT      NOT NULL CHECK (total_satang > 0),
    note         TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL REFERENCES users (line_user_id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX bills_group_idx ON bills (group_id, created_at DESC);

CREATE TABLE bill_shares (
    bill_id      UUID   NOT NULL REFERENCES bills (id) ON DELETE CASCADE,
    user_id      TEXT   NOT NULL REFERENCES users (line_user_id),
    share_satang BIGINT NOT NULL CHECK (share_satang >= 0),
    PRIMARY KEY (bill_id, user_id)
);

CREATE INDEX bill_shares_user_idx ON bill_shares (user_id);

-- A recorded transfer between two members. Settlements are ordinary ledger
-- entries rather than a flag on the bills they clear: money moves between
-- people, not between line items, and one transfer often settles many bills.
CREATE TABLE settlements (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_id      UUID        NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    from_user     TEXT        NOT NULL REFERENCES users (line_user_id),
    to_user       TEXT        NOT NULL REFERENCES users (line_user_id),
    amount_satang BIGINT      NOT NULL CHECK (amount_satang > 0),
    note          TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_user <> to_user)
);

CREATE INDEX settlements_group_idx ON settlements (group_id, created_at DESC);

-- +goose Down
DROP TABLE settlements;
DROP TABLE bill_shares;
DROP TABLE bills;
DROP TABLE group_members;
DROP TABLE groups;
DROP TABLE users;
