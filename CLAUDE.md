# billsplit-api

Go API for a LINE bill-splitting LIFF app. The frontend is a separate repo
(`../frontend`) and calls this over CORS.

Fiber v2 · pgx v5 · Postgres 17 · Go 1.26

## The team loop

Work moves through five roles, defined in `.claude/agents/`:

```
po  →  junior-go  →  senior-go  →  (changes requested? back to junior-go)
                                →  PASS  →  security + qa-adversarial
                                                 →  findings? back to junior-go
                                                 →  clean  →  po signs off
```

`security` and `qa-adversarial` run together after PASS and look for different
things: QA hunts the input nobody imagined, security hunts the group member who
wants their own debt to shrink. `ux-designer` belongs to the frontend repo but
is worth pulling in whenever a change alters a screen.

- `po` opens the change: who the user is, what hurts today, and acceptance
  criteria written as behaviour a person would notice. Nothing starts without
  this, because a task with no stated user produces code nobody needed.
- `junior-go` writes the code and its tests, and runs `make check`.
- `senior-go` reviews and returns `PASS` or `CHANGES REQUESTED` with a numbered
  list. It does not write the fix.
- The loop repeats until `PASS`. Three round trips on one change means the task
  was underspecified — stop, and send it back to `po` rather than looping a
  fourth time.
- `qa-adversarial` runs only after `PASS`, and hunts for what nobody designed
  for. Its findings re-enter at `junior-go`.
- `po` closes the change: `SHIPS` or `NEEDS WORK`, judged against the
  acceptance criteria and against how it actually feels to use.

A change is done when `senior-go` says PASS, `qa-adversarial` has nothing
CRITICAL or HIGH, and `po` says SHIPS. Correct code that does not help the user
is not done.

## Rules this codebase holds itself to

Reviews are graded against these, not against personal taste.

### Money

Every amount is `money.Satang` — an `int64` count of 1/100 baht. There is no
`float64` in this repo and there must not be: `0.1 + 0.2 != 0.3` in binary
floating point, and a bill splitter that loses satang loses its users.

Amounts cross HTTP as **strings**, not JSON numbers. A JSON number is an IEEE
754 double in every browser, so `1234.55` can arrive as `1234.5499999999999`
before the server sees it. Parse with `money.ParseBaht`.

Splitting uses `money.SplitEqual` or `money.SplitByWeight`. Both guarantee the
shares sum exactly to the total; the odd satang goes to the front of the
participant list. Hand-entered shares go through `money.ValidateShares`.

### Auth

The client sends the LIFF ID token as a bearer token. `middleware.Auth` verifies
it with LINE **and pins `aud` to our own channel** — without that check, a token
minted for someone else's LIFF app would let its holder act as any user here.

`liff.getProfile()` on the client is display only. The only trustworthy identity
is `middleware.CurrentUser(c)`.

Verified tokens are cached for 5 minutes, keyed by the SHA-256 of the token —
never the raw token, which would sit in heap dumps in plaintext.

### Layering

- All SQL lives in `internal/repo`. Handlers never write queries; repo functions
  never write HTTP responses.
- Authorisation is folded into the query's `WHERE` clause, not checked
  separately afterwards. A separate check is a step a future handler can forget.
- Every group-scoped handler begins with `h.requireMember(c)`.
- A non-member gets **404, not 403**. A 403 confirms the group exists, which
  lets anyone probe for real group IDs.
- Multi-statement writes run in one transaction with `defer tx.Rollback(ctx)`.
  A bill without its shares corrupts every balance in the group.

### Errors

`fiber.NewError(status, msg)` is the only error whose message reaches the
client. Everything else is logged in full and returned as a bare 500 — query
errors name tables, columns, and hosts.

A missing row is `repo.ErrNotFound`, compared with `errors.Is`.

### Comments

Explain the non-obvious decision, not the statement. `// increment i` is noise;
`// the odd satang goes to the payer so the same person is not always short`
is the reason someone will need in six months.

## Layout

```
cmd/server/          main, routing, graceful shutdown
internal/money/      Satang + split algorithms        (unit tested)
internal/settle/     debt minimization                (property tested)
internal/line/       ID token verify, Flex, push, webhook signature
internal/repo/       every SQL query in the project   (integration tested)
internal/handler/    HTTP handlers
internal/middleware/ auth
migrations/          goose
```

## Commands

```bash
make db        # start Postgres on :5433
make migrate   # apply migrations (needs goose)
make run       # API on :8080
make check     # gofmt + vet + tests — the review gate
make itest     # tests that need a live database
```

## Not built yet

- Webhook endpoint. `line.ValidateSignature` exists and is tested; no route
  calls it.

  **This is a security gap, not just a missing feature.** `POST /groups` takes
  `lineGroupId` from the body on a route `requireMember` cannot guard, so the
  only checks available today are on the value's *shape*: it must look like a
  LINE group or room ID (`C` or `R` plus 32 lowercase hex digits), the group
  name is length-capped because it reaches a lock screen through the push
  `altText`, and a group with no bills cannot be summarised into a chat at all.

  What none of that proves is that the caller is actually in the chat they
  named. Someone who learns a chat ID can still claim it before its real members
  first open the app; `line_group_id` is `UNIQUE` and nothing rebinds or deletes
  a group, so those members are then silently enrolled into a stranger's group,
  permanently. Closing it needs the Messaging API to confirm chat membership,
  which needs this webhook. Build it before this app is used by anyone who is
  not a friend of the author.
- Editing a bill. Deleting one exists (`DELETE /groups/:id/bills/:billId`,
  author only) because "exact" mode lets a member attach any share to another
  member and reversibility is the only defence — a real dinner can legitimately
  be large, so a cap is not.

  Reversibility is not symmetric, though, and the delete is guarded because of
  it. One member can own both rows of a self-cancelling pair — a bill naming
  someone else as payer, plus a settlement clearing the debt it invents — and
  retracting only the bill leaves the victim owing money for a payment that
  never happened, with no endpoint they can use to undo it. So a bill cannot be
  deleted while any member would be left net-positive on settlements alone; that
  is a **409**, and the fix is for the settlement's sender to withdraw it first.
  The honest cost: record a bill, get paid for it, then want to correct a typo,
  and you must ask the payer to withdraw and re-record. Accepted, because bill
  editing does not exist yet and the alternative is an unrecoverable theft.
- Rate limiting.
- Pagination on bill and settlement lists.

## Known gaps in what is built

- A settlement is bounded by `min(what the sender owes the group, what the group
  owes the recipient)` *at the moment the request is handled* — neither party may
  cross zero. The bound is the pair's net positions, not the transfer plan: the
  plan is only one of several ways to flatten the same nets, and a payment that
  really happened between two members the plan did not pair must still be
  recordable.

  Two settlements posted concurrently are each checked against the same
  pre-transaction balances, so a determined member can overpay by racing
  themselves. The window is small and the result is reversible via `DELETE
  /groups/:id/settlements/:settlementId`; closing it properly means computing the
  balance inside the inserting transaction.
