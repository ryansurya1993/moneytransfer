# Ledger check

The relay checks every new ledger entry and every finished transfer against the money rules. When a
rule breaks, it records a finding and holds that merchant's payouts until a person closes the
finding. Why it is built this way: [ADR 9](adr/0009-incremental-ledger-check.md).

| Rule | Broken when |
|---|---|
| `VERSION_GAP` | an entry's merchant version doesn't follow the previous one |
| `BALANCE_CHAIN` | an entry's `balance_after` isn't the previous balance plus its amount |
| `MERCHANT_BALANCE` | the merchant's balance or version differs from its newest entry |
| `DEBIT_AMOUNT`, `DEBIT_MISSING` | a finished transfer's debit is the wrong amount, or missing |
| `REFUND_AMOUNT`, `REFUND_NOT_FAILED`, `REFUND_MISSING` | a refund is the wrong amount, belongs to a transfer that didn't fail, or is missing for one that did |

While a merchant is held, new transfers get `422 MERCHANT_PAYOUTS_HELD` and nothing is stored,
accepted transfers wait as `PENDING`, and transfers already at the provider still finish.

## Try it

The service never breaks these rules on its own, so the way to see the check work is to change the
balance by hand, as a bug would. With the stack running, in bash (on Windows, Git Bash, not
PowerShell):

1. Add 1,000 rupiah to the balance without a ledger entry:

   ```bash
   docker compose exec postgres psql -U moneytransfer -c "UPDATE merchants SET balance = balance + 1000 WHERE id = 1;"
   ```

2. Within about 30 seconds the relay logs the alert (no amounts, only ids and rules):

   ```bash
   docker compose logs relay | grep ledger_mismatch
   ```

   ```
   {"time":"...","level":"ERROR","msg":"ledger findings need a person","alert":"ledger_mismatch","severity":"critical","open_findings":1,"merchant_ids":[1],"rules":["MERCHANT_BALANCE"]}
   ```

3. New transfers are declined:

   ```bash
   curl -s -X POST localhost:18080/transfer \
     -H 'Content-Type: application/json' \
     -H 'Idempotency-Key: drill-1' \
     -d '{"amount":150000,"currency":"IDR","bank_code":"BCA","account_number":"1234566789","account_holder_name":"Budi Santoso"}'
   ```

   ```
   {"type":"about:blank","title":"Unprocessable Entity","status":422,
    "detail":"Payouts for this merchant are on hold. Contact support.","code":"MERCHANT_PAYOUTS_HELD",...}
   ```

4. Find the open finding:

   ```bash
   docker compose exec postgres psql -U moneytransfer -c \
     "SELECT id, merchant_id, rule, merchant_version, transfer_id, found_at FROM ledger_findings WHERE resolved_at IS NULL;"
   ```

5. Undo the change:

   ```bash
   docker compose exec postgres psql -U moneytransfer -c "UPDATE merchants SET balance = balance - 1000 WHERE id = 1;"
   ```

6. Close the finding, with who and why (both are required and stored with it):

   ```bash
   docker compose run --rm --no-deps relay relay resolve-ledger-finding <id> --by <name> --note "manual balance edit reverted"
   ```

   ```
   finding 1 resolved; payouts for merchant 1 are not on hold
   ```

7. Send the request from step 3 again: `202`, and the transfer goes through.

## When the alert fires

1. List the open findings (step 4) and note the merchant, rule, version and transfer of each.
2. Look at the entries around that version, and at the transfer if one is named:

   ```bash
   docker compose exec postgres psql -U moneytransfer -c \
     "SELECT merchant_version, type, amount, balance_after, transfer_id FROM balance_entries WHERE merchant_id = 1 ORDER BY merchant_version DESC LIMIT 10;"
   ```

3. Find and fix the cause first. In production, never fix it by editing rows by hand: that skips
   the ledger entry and the version that keep the books explainable.
   The drill above only undoes its own edit.
4. Then close each finding with the resolve command (step 6). The merchant's payouts resume when its
   last open finding is closed.

## Known limit

The resolve command doesn't re-check the ledger. Closing a finding while the balance is still wrong
lifts the hold: the check doesn't record the same problem twice, so it stays quiet until the next
balance change breaks the chain, and then holds the merchant again (19 seconds after the next
transfer, in a test run). Until then, transfers can go out on the wrong balance. Fix first, then
resolve. A resolve command that re-checks and refuses while the books still don't match is listed
under future improvements.
