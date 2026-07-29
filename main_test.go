package main

import (
	"strings"
	"testing"

	"github.com/koinvote/event-verifier/service"
)

// The bundled reports must both verify: one in the current BTC-Time format,
// one predating it. Version 1.0.0 broke on a report whose columns had shifted
// and nobody noticed for months, so the compatibility promise is worth
// asserting rather than assuming.
func TestBundledReportsVerify(t *testing.T) {
	for _, path := range []string{"example.csv", "example-legacy.csv"} {
		t.Run(path, func(t *testing.T) {
			parsed, err := loadReport(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}

			result, err := service.NewLotteryService().Compute(service.LotteryContext{
				Seed:        parsed.header.seed,
				PoolSatoshi: parsed.header.poolSatoshi,
				WinnerCount: parsed.header.winnerCount,
				Balances:    parsed.balances,
				Params: service.LotteryParams{
					PlatformFeePercentage: parsed.header.platformFeePercentage,
					DustThresholdSatoshi:  parsed.header.dustThresholdSatoshi,
					PayoutFeeMultiplier:   parsed.header.payoutFeeMultiplier,
					FeeRateSatVb:          parsed.header.feeRateSatVb,
					TxOverheadVBytes:      parsed.header.txOverheadVBytes,
					InputP2WSHBytes:       parsed.header.inputP2WSHBytes,
					OutputDefaultVBytes:   parsed.header.outputDefaultVBytes,
				},
			})
			if err != nil {
				t.Fatalf("compute: %v", err)
			}

			if issues := compareResults(parsed, result); len(issues) != 0 {
				for _, issue := range issues {
					t.Errorf("unexpected mismatch: %s", issue)
				}
			}
		})
	}
}

// Columns are located by name, so inserting one must not disturb the fields
// around it. This is the exact shape of the defect that broke 1.0.0: a
// referral_code column appeared and every later field was read one place off.
func TestColumnsAreFoundByNameNotPosition(t *testing.T) {
	names := []string{"type", "address", "referral_code", "holding_score_sat_blocks"}
	row := []string{"balance", "bc1qexample", "KOIN2026", "296400000000"}
	cols := newColumns(names)

	if got := cols.require(row, "balance", "address"); got != "bc1qexample" {
		t.Errorf("address = %q, want bc1qexample", got)
	}
	if got := cols.require(row, "balance", "holding_score_sat_blocks", "balance_satoshi"); got != "296400000000" {
		t.Errorf("weight = %q, want 296400000000", got)
	}
}

// A report from a newer backend must say so plainly, rather than being
// reported as a payout that fails to verify.
func TestMissingColumnNamesItself(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic naming the missing column")
		}
		err, ok := r.(error)
		if !ok {
			t.Fatalf("panic value %v is not an error", r)
		}
		got := err.Error()
		if !strings.Contains(got, "original_reward_satoshi") || !strings.Contains(got, "older than the report") {
			t.Errorf("unhelpful message: %s", got)
		}
	}()

	cols := newColumns([]string{"type", "address"})
	cols.require([]string{"result_winner", "bc1qexample"}, "result_winner", "original_reward_satoshi")
}
