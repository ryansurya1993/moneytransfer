package transfer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ryansurya1993/moneytransfer/internal/transfer"
)

// fakeLedgerCheck holds one or more merchants' ledgers and records what the check writes.
type fakeLedgerCheck struct {
	due         []transfer.MerchantID
	dueErr      error
	checkpoints map[transfer.MerchantID]transfer.LedgerCheckpoint
	lockedElse  map[transfer.MerchantID]bool // held by another transaction
	balances    map[transfer.MerchantID]transfer.BalanceChange
	balanceErr  map[transfer.MerchantID]error
	entries     map[transfer.MerchantID][]transfer.LedgerEntry
	transfers   map[uuid.UUID]transfer.Transfer
	finished    []transfer.FinishedTransfer

	findings []transfer.LedgerFinding
	held     map[transfer.MerchantID]bool
	checked  []uuid.UUID
	resolved map[int64]string // finding ID -> "by: note"
}

func newFakeLedgerCheck() *fakeLedgerCheck {
	return &fakeLedgerCheck{
		checkpoints: map[transfer.MerchantID]transfer.LedgerCheckpoint{},
		lockedElse:  map[transfer.MerchantID]bool{},
		balances:    map[transfer.MerchantID]transfer.BalanceChange{},
		balanceErr:  map[transfer.MerchantID]error{},
		entries:     map[transfer.MerchantID][]transfer.LedgerEntry{},
		transfers:   map[uuid.UUID]transfer.Transfer{},
		held:        map[transfer.MerchantID]bool{},
		resolved:    map[int64]string{},
	}
}

func (f *fakeLedgerCheck) DueMerchants(context.Context, int) ([]transfer.MerchantID, error) {
	return f.due, f.dueErr
}

func (f *fakeLedgerCheck) LockCheckpoint(_ context.Context, id transfer.MerchantID) (transfer.LedgerCheckpoint, bool, error) {
	if f.lockedElse[id] {
		return transfer.LedgerCheckpoint{}, false, nil
	}
	cp, ok := f.checkpoints[id]
	if !ok {
		cp = transfer.LedgerCheckpoint{MerchantID: id}
	}
	return cp, true, nil
}

func (f *fakeLedgerCheck) Balance(_ context.Context, id transfer.MerchantID) (transfer.BalanceChange, error) {
	return f.balances[id], f.balanceErr[id]
}

func (f *fakeLedgerCheck) EntriesAfter(_ context.Context, id transfer.MerchantID, after, upTo int64, limit int) ([]transfer.LedgerEntry, error) {
	var out []transfer.LedgerEntry
	for _, e := range f.entries[id] {
		if e.MerchantVersion > after && e.MerchantVersion <= upTo && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeLedgerCheck) Transfers(_ context.Context, ids []uuid.UUID) ([]transfer.Transfer, error) {
	var out []transfer.Transfer
	for _, id := range ids {
		if t, ok := f.transfers[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeLedgerCheck) SaveCheckpoint(_ context.Context, cp transfer.LedgerCheckpoint) error {
	f.checkpoints[cp.MerchantID] = cp
	return nil
}

func (f *fakeLedgerCheck) UncheckedFinished(_ context.Context, limit int) ([]transfer.FinishedTransfer, error) {
	var out []transfer.FinishedTransfer
	for _, t := range f.finished {
		if !slices.Contains(f.checked, t.ID) && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeLedgerCheck) MarkChecked(_ context.Context, ids []uuid.UUID) error {
	f.checked = append(f.checked, ids...)
	return nil
}

func (f *fakeLedgerCheck) AddFindings(_ context.Context, fs []transfer.LedgerFinding) ([]transfer.LedgerFinding, error) {
	var added []transfer.LedgerFinding
	for _, nf := range fs {
		if slices.ContainsFunc(f.findings, func(old transfer.LedgerFinding) bool { return sameProblem(old, nf) }) {
			continue
		}
		nf.ID = int64(len(f.findings) + 1)
		f.findings = append(f.findings, nf)
		added = append(added, nf)
	}
	return added, nil
}

func sameProblem(a, b transfer.LedgerFinding) bool {
	a.ID, b.ID = 0, 0
	return a == b
}

func (f *fakeLedgerCheck) HoldPayouts(_ context.Context, ids []transfer.MerchantID) ([]transfer.MerchantID, error) {
	var newly []transfer.MerchantID
	for _, id := range ids {
		if !f.held[id] {
			f.held[id] = true
			newly = append(newly, id)
		}
	}
	return newly, nil
}

func (f *fakeLedgerCheck) OpenFindings(context.Context, int) (transfer.OpenFindings, error) {
	var open transfer.OpenFindings
	for _, fd := range f.findings {
		if _, done := f.resolved[fd.ID]; done {
			continue
		}
		open.Count++
		if !slices.Contains(open.MerchantIDs, fd.MerchantID) {
			open.MerchantIDs = append(open.MerchantIDs, fd.MerchantID)
		}
		if !slices.Contains(open.Rules, fd.Rule) {
			open.Rules = append(open.Rules, fd.Rule)
		}
	}
	return open, nil
}

func (f *fakeLedgerCheck) Resolve(_ context.Context, id int64, by, note string) (transfer.MerchantID, error) {
	if id < 1 || int(id) > len(f.findings) {
		return 0, transfer.ErrFindingNotFound
	}
	if _, done := f.resolved[id]; done {
		return 0, transfer.ErrFindingResolved
	}
	f.resolved[id] = by + ": " + note
	return f.findings[id-1].MerchantID, nil
}

func (f *fakeLedgerCheck) ReleaseHold(_ context.Context, id transfer.MerchantID) (bool, error) {
	for _, fd := range f.findings {
		if _, done := f.resolved[fd.ID]; fd.MerchantID == id && !done {
			return f.held[id], nil
		}
	}
	f.held[id] = false
	return false, nil
}

const merchant transfer.MerchantID = 1

// ledgerOf builds a merchant ledger: an opening entry, then a debit per transfer and a refund for FAILED ones.
// It returns the entries and the merchant row that matches them.
func ledgerOf(opening transfer.Amount, ts ...transfer.Transfer) ([]transfer.LedgerEntry, transfer.BalanceChange) {
	entries := []transfer.LedgerEntry{{MerchantID: merchant, Type: "OPENING", Amount: opening, BalanceAfter: opening, MerchantVersion: 1}}
	add := func(id uuid.UUID, typ transfer.EntryType, amount transfer.Amount) {
		last := entries[len(entries)-1]
		entries = append(entries, transfer.LedgerEntry{
			MerchantID:      merchant,
			TransferID:      id,
			Type:            typ,
			Amount:          amount,
			BalanceAfter:    last.BalanceAfter + amount,
			MerchantVersion: last.MerchantVersion + 1,
		})
	}
	for _, t := range ts {
		add(t.ID, transfer.EntryDebit, -t.Amount)
		if t.Status == transfer.StatusFailed {
			add(t.ID, transfer.EntryRefund, t.Amount)
		}
	}
	last := entries[len(entries)-1]
	return entries, transfer.BalanceChange{Balance: last.BalanceAfter, Version: last.MerchantVersion}
}

func newTransfer(amount transfer.Amount, status transfer.Status) transfer.Transfer {
	return transfer.Transfer{ID: uuid.New(), MerchantID: merchant, Amount: amount, Status: status}
}

func newChecker(f *fakeLedgerCheck, logs io.Writer) (*transfer.LedgerChecker, *fakeDB) {
	db := newFakeDB(0)
	db.ledgerCheck = f
	return transfer.NewLedgerChecker(db, slog.New(slog.NewJSONHandler(logs, nil))), db
}

type findingKey struct {
	rule     transfer.LedgerRule
	version  int64
	transfer uuid.UUID
}

func keysOf(fs []transfer.LedgerFinding) []findingKey {
	keys := make([]findingKey, len(fs))
	for i, f := range fs {
		keys[i] = findingKey{f.Rule, f.MerchantVersion, f.TransferID}
	}
	return keys
}

func TestLedgerCheckFindsEachBrokenRule(t *testing.T) {
	paid := newTransfer(300, transfer.StatusSuccess)
	failed := newTransfer(200, transfer.StatusFailed)

	tests := []struct {
		name    string
		corrupt func(entries []transfer.LedgerEntry, b *transfer.BalanceChange, ts map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry
		want    func(entries []transfer.LedgerEntry, b transfer.BalanceChange) []findingKey
	}{
		{
			name: "clean ledger",
			corrupt: func(e []transfer.LedgerEntry, _ *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				return e
			},
			want: func([]transfer.LedgerEntry, transfer.BalanceChange) []findingKey { return nil },
		},
		{
			name: "missing entry leaves a version gap",
			corrupt: func(e []transfer.LedgerEntry, _ *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				return slices.Delete(e, 1, 2) // paid's debit
			},
			want: func(e []transfer.LedgerEntry, _ transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleVersionGap, 3, uuid.Nil}}
			},
		},
		{
			name: "edited entry breaks the balance chain",
			corrupt: func(e []transfer.LedgerEntry, _ *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				e[2].BalanceAfter += 50
				return e
			},
			want: func([]transfer.LedgerEntry, transfer.BalanceChange) []findingKey {
				// An edited balance breaks the link into the entry and out of it.
				return []findingKey{{transfer.RuleBalanceChain, 3, uuid.Nil}, {transfer.RuleBalanceChain, 4, uuid.Nil}}
			},
		},
		{
			name: "balance changed without an entry",
			corrupt: func(e []transfer.LedgerEntry, b *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				b.Balance += 1000
				return e
			},
			want: func(_ []transfer.LedgerEntry, b transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleMerchantBalance, b.Version, uuid.Nil}}
			},
		},
		{
			name: "version moved without an entry",
			corrupt: func(e []transfer.LedgerEntry, b *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				b.Version++
				return e
			},
			want: func(_ []transfer.LedgerEntry, b transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleMerchantBalance, b.Version, uuid.Nil}}
			},
		},
		{
			name: "debit of the wrong amount",
			corrupt: func(e []transfer.LedgerEntry, _ *transfer.BalanceChange, ts map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				p := ts[paid.ID]
				p.Amount = 301
				ts[paid.ID] = p
				return e
			},
			want: func([]transfer.LedgerEntry, transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleDebitAmount, 2, paid.ID}}
			},
		},
		{
			name: "refund of a paid transfer",
			corrupt: func(e []transfer.LedgerEntry, _ *transfer.BalanceChange, ts map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				f := ts[failed.ID]
				f.Status = transfer.StatusSuccess
				ts[failed.ID] = f
				return e
			},
			want: func([]transfer.LedgerEntry, transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleRefundNotFailed, 4, failed.ID}}
			},
		},
		{
			name: "refund of the wrong amount",
			corrupt: func(e []transfer.LedgerEntry, b *transfer.BalanceChange, _ map[uuid.UUID]transfer.Transfer) []transfer.LedgerEntry {
				e[3].Amount += 10
				e[3].BalanceAfter += 10
				b.Balance += 10
				return e
			},
			want: func([]transfer.LedgerEntry, transfer.BalanceChange) []findingKey {
				return []findingKey{{transfer.RuleRefundAmount, 4, failed.ID}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, balance := ledgerOf(1000, paid, failed)
			ts := map[uuid.UUID]transfer.Transfer{paid.ID: paid, failed.ID: failed}
			entries = tt.corrupt(entries, &balance, ts)

			f := newFakeLedgerCheck()
			f.due = []transfer.MerchantID{merchant}
			f.balances[merchant] = balance
			f.entries[merchant] = entries
			f.transfers = ts
			c, _ := newChecker(f, io.Discard)

			busy, err := c.Check(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			if busy {
				t.Error("Check reported more work for a small ledger")
			}
			if got, want := keysOf(f.findings), tt.want(entries, balance); !slices.Equal(got, want) {
				t.Errorf("findings = %+v, want %+v", got, want)
			}
			if got, want := f.held[merchant], len(f.findings) > 0; got != want {
				t.Errorf("payouts held = %t, want %t", got, want)
			}
			last := entries[len(entries)-1]
			want := transfer.LedgerCheckpoint{MerchantID: merchant, Version: last.MerchantVersion, BalanceAfter: last.BalanceAfter}
			if got := f.checkpoints[merchant]; got != want {
				t.Errorf("checkpoint = %+v, want %+v", got, want)
			}
		})
	}
}

func TestLedgerCheckReadsOnlyEntriesAfterTheCheckpoint(t *testing.T) {
	entries, balance := ledgerOf(1000, newTransfer(100, transfer.StatusSuccess), newTransfer(100, transfer.StatusSuccess))
	entries[1].BalanceAfter = 1 // already verified once; a later run must not look at it again
	f := newFakeLedgerCheck()
	f.due = []transfer.MerchantID{merchant}
	f.balances[merchant] = balance
	f.entries[merchant] = entries
	f.checkpoints[merchant] = transfer.LedgerCheckpoint{MerchantID: merchant, Version: 2, BalanceAfter: 900}
	for _, e := range entries[1:] {
		f.transfers[e.TransferID] = transfer.Transfer{ID: e.TransferID, Amount: 100, Status: transfer.StatusSuccess}
	}
	c, _ := newChecker(f, io.Discard)

	if _, err := c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}

	if len(f.findings) != 0 {
		t.Errorf("findings = %+v, want none", f.findings)
	}
}

func TestLedgerCheckStopsAtTheEntryLimitAndReportsMoreWork(t *testing.T) {
	ts := make([]transfer.Transfer, 10000)
	f := newFakeLedgerCheck()
	for i := range ts {
		ts[i] = newTransfer(1, transfer.StatusSuccess)
		f.transfers[ts[i].ID] = ts[i]
	}
	entries, balance := ledgerOf(20000, ts...)
	balance.Balance += 5 // would be a finding, but the newest entries aren't read in this round
	f.due = []transfer.MerchantID{merchant}
	f.balances[merchant] = balance
	f.entries[merchant] = entries
	c, _ := newChecker(f, io.Discard)

	busy, err := c.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if !busy {
		t.Error("Check reported no more work after hitting the entry limit")
	}
	if len(f.findings) != 0 {
		t.Errorf("findings = %+v, want none before all entries are read", f.findings)
	}
	if got := f.checkpoints[merchant].Version; got != 10000 {
		t.Errorf("checkpoint version = %d, want 10000", got)
	}

	if _, err := c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := keysOf(f.findings); !slices.Equal(got, []findingKey{{transfer.RuleMerchantBalance, balance.Version, uuid.Nil}}) {
		t.Errorf("findings after the second round = %+v, want the balance mismatch", got)
	}
}

// Merchants with a mismatch nobody has fixed stay due on every round; a full list of them is no reason to hurry.
func TestLedgerCheckDoesNotSpinOnUnfixedMismatches(t *testing.T) {
	f := newFakeLedgerCheck()
	for id := range transfer.MerchantID(1000) {
		f.due = append(f.due, id+1)
		f.balances[id+1] = transfer.BalanceChange{Balance: 1, Version: 1}
	}
	c, _ := newChecker(f, io.Discard)

	busy, err := c.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if busy {
		t.Error("Check reported more work although it read no entries")
	}
}

func TestLedgerCheckSkipsAMerchantAnotherCheckHolds(t *testing.T) {
	entries, balance := ledgerOf(1000)
	f := newFakeLedgerCheck()
	f.due = []transfer.MerchantID{merchant}
	f.balances[merchant] = balance
	f.entries[merchant] = entries
	f.lockedElse[merchant] = true
	c, _ := newChecker(f, io.Discard)

	if _, err := c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, ok := f.checkpoints[merchant]; ok {
		t.Error("checkpoint saved for a merchant another check holds")
	}
}

func TestLedgerCheckKeepsGoingAfterOneMerchantFails(t *testing.T) {
	f := newFakeLedgerCheck()
	f.due = []transfer.MerchantID{1, 2}
	f.balanceErr[1] = errors.New("boom")
	entries, balance := ledgerOf(1000)
	for i := range entries {
		entries[i].MerchantID = 2
	}
	f.entries[2] = entries
	f.balances[2] = balance
	c, _ := newChecker(f, io.Discard)

	_, err := c.Check(t.Context())

	if err == nil || !strings.Contains(err.Error(), "merchant 1") {
		t.Errorf("Check error = %v, want one naming merchant 1", err)
	}
	if _, ok := f.checkpoints[2]; !ok {
		t.Error("merchant 2 wasn't checked after merchant 1 failed")
	}
}

func TestLedgerCheckFindsFinishedTransfersWithoutTheirEntries(t *testing.T) {
	ok := transfer.FinishedTransfer{ID: uuid.New(), MerchantID: 1, Status: transfer.StatusSuccess, HasDebit: true}
	refunded := transfer.FinishedTransfer{ID: uuid.New(), MerchantID: 1, Status: transfer.StatusFailed, HasDebit: true, HasRefund: true}
	noDebit := transfer.FinishedTransfer{ID: uuid.New(), MerchantID: 2, Status: transfer.StatusSuccess}
	noRefund := transfer.FinishedTransfer{ID: uuid.New(), MerchantID: 3, Status: transfer.StatusFailed, HasDebit: true}
	f := newFakeLedgerCheck()
	f.finished = []transfer.FinishedTransfer{ok, refunded, noDebit, noRefund}
	c, _ := newChecker(f, io.Discard)

	if _, err := c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}

	want := []findingKey{{transfer.RuleDebitMissing, 0, noDebit.ID}, {transfer.RuleRefundMissing, 0, noRefund.ID}}
	if got := keysOf(f.findings); !slices.Equal(got, want) {
		t.Errorf("findings = %+v, want %+v", got, want)
	}
	if !f.held[2] || !f.held[3] || f.held[1] {
		t.Errorf("held = %v, want merchants 2 and 3 only", f.held)
	}
	if got := len(f.checked); got != 4 {
		t.Errorf("%d transfers marked checked, want 4", got)
	}
}

func TestLedgerCheckRecordsAProblemOnce(t *testing.T) {
	entries, balance := ledgerOf(1000)
	balance.Balance++
	f := newFakeLedgerCheck()
	f.due = []transfer.MerchantID{merchant}
	f.balances[merchant] = balance
	f.entries[merchant] = entries
	var logs bytes.Buffer
	c, _ := newChecker(f, &logs)

	for range 3 {
		if _, err := c.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	if len(f.findings) != 1 {
		t.Errorf("%d findings, want 1", len(f.findings))
	}
	if n := strings.Count(logs.String(), "ledger finding recorded"); n != 1 {
		t.Errorf("finding logged %d times, want once", n)
	}
	if n := strings.Count(logs.String(), "merchant payouts held"); n != 1 {
		t.Errorf("hold logged %d times, want once", n)
	}
}

func TestLedgerAlertNamesMerchantsAndRulesButNoAmounts(t *testing.T) {
	f := newFakeLedgerCheck()
	f.findings = []transfer.LedgerFinding{
		{ID: 1, MerchantID: 7, Rule: transfer.RuleBalanceChain, MerchantVersion: 12},
		{ID: 2, MerchantID: 7, Rule: transfer.RuleRefundMissing, TransferID: uuid.New()},
	}
	var logs syncBuffer
	c, _ := newChecker(f, &logs)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { return strings.Contains(logs.String(), "ledger_mismatch") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var alert map[string]any
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "ledger_mismatch") {
			if err := json.Unmarshal([]byte(line), &alert); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]any{
		"alert":         "ledger_mismatch",
		"severity":      "critical",
		"open_findings": float64(2),
		"merchant_ids":  []any{float64(7)},
		"rules":         []any{"BALANCE_CHAIN", "REFUND_MISSING"},
	}
	for k, v := range want {
		got, _ := json.Marshal(alert[k])
		wantJSON, _ := json.Marshal(v)
		if !bytes.Equal(got, wantJSON) {
			t.Errorf("alert %s = %s, want %s", k, got, wantJSON)
		}
	}
	for _, k := range []string{"balance", "amount", "balance_after"} {
		if _, ok := alert[k]; ok {
			t.Errorf("alert has %q; amounts stay in the database", k)
		}
	}
}

func TestFailedLedgerCheckRoundStillCountsAsFinished(t *testing.T) {
	f := newFakeLedgerCheck()
	f.dueErr = errors.New("database is down")
	var logs syncBuffer
	c, _ := newChecker(f, &logs)
	start := c.LastRound()
	// The Windows clock can return the same time for a while; let it move past the start first.
	for !time.Now().After(start) {
	}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error)
	go func() { done <- c.Run(ctx) }()
	waitFor(t, func() bool { return c.LastRound().After(start) })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(logs.String(), "database is down") {
		t.Errorf("logs = %s, want the round's error", logs.String())
	}
}

func TestResolveLedgerFinding(t *testing.T) {
	f := newFakeLedgerCheck()
	f.findings = []transfer.LedgerFinding{
		{ID: 1, MerchantID: 7, Rule: transfer.RuleBalanceChain, MerchantVersion: 3},
		{ID: 2, MerchantID: 7, Rule: transfer.RuleVersionGap, MerchantVersion: 5},
	}
	f.held[7] = true
	db := newFakeDB(0)
	db.ledgerCheck = f

	if _, _, err := transfer.ResolveLedgerFinding(t.Context(), db, 1, " ", "checked"); err == nil {
		t.Error("resolved without saying who")
	}
	if _, _, err := transfer.ResolveLedgerFinding(t.Context(), db, 1, "ops", ""); err == nil {
		t.Error("resolved without saying why")
	}

	m, held, err := transfer.ResolveLedgerFinding(t.Context(), db, 1, "ops", "entry 3 matches the bank statement")
	if err != nil {
		t.Fatal(err)
	}
	if m != 7 || !held {
		t.Errorf("first resolve = merchant %d, held %t; want merchant 7 still held", m, held)
	}
	if _, held, err = transfer.ResolveLedgerFinding(t.Context(), db, 2, "ops", "replayed entry 5"); err != nil || held {
		t.Errorf("last resolve = held %t, err %v; want released", held, err)
	}
	if _, _, err := transfer.ResolveLedgerFinding(t.Context(), db, 2, "ops", "again"); !errors.Is(err, transfer.ErrFindingResolved) {
		t.Errorf("second resolve of the same finding = %v, want ErrFindingResolved", err)
	}
	if f.resolved[1] != "ops: entry 3 matches the bank statement" {
		t.Errorf("stored resolution = %q", f.resolved[1])
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
