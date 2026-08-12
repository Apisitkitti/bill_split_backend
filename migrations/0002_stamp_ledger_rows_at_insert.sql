-- +goose Up

-- The delete guard in repo.DeleteBill asks whether a settlement is younger than
-- the bill being withdrawn, and answers it by comparing created_at. now() is the
-- transaction's *start* time, which is the wrong instant for that question: what
-- decides whether a settlement could have been justified by a bill is whether it
-- saw that bill, and visibility follows commit order, not BEGIN order.
--
-- CreateBill takes no group lock, so a settlement can BEGIN, block on the group
-- lock while a bill commits, then be admitted against a ledger that now contains
-- that bill — while carrying a now() stamp from before the bill existed. The
-- guard then reads it as older and lets the bill be deleted out from under it,
-- which is the unrecoverable case the guard exists to refuse.
--
-- clock_timestamp() is read when the row is inserted, which is necessarily after
-- the reads that admitted it. That restores the chain the guard depends on:
--
--   bill stamped < bill commits < settlement's ledger read < settlement stamped
--
-- so "the settlement saw the bill" now implies "the settlement is stamped
-- later". Only these two tables need it; nothing compares the other created_at
-- columns against each other.
ALTER TABLE bills       ALTER COLUMN created_at SET DEFAULT clock_timestamp();
ALTER TABLE settlements ALTER COLUMN created_at SET DEFAULT clock_timestamp();

-- +goose Down

-- Restoring the default is all the Down can do, and all it should: it changes
-- what future inserts are stamped with and deliberately leaves existing rows
-- alone. Rewriting them is not possible anyway — the instant a committed row was
-- inserted at is not recorded anywhere else — and it is not needed, because both
-- functions return wall-clock time from the same server and the rows stay
-- comparable across the change. Going down only reopens the race for rows
-- written afterwards.
ALTER TABLE bills       ALTER COLUMN created_at SET DEFAULT now();
ALTER TABLE settlements ALTER COLUMN created_at SET DEFAULT now();
