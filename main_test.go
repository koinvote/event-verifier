package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koinvote/event-verifier/service"
)

// Every bundled report must verify, across all three formats this tool has
// been asked to read: the current one, the one before the fee model changed,
// and the one predating BTC-Time scoring. Version 1.0.0 broke on a report
// whose columns had shifted and nobody noticed for months, so the
// compatibility promise is worth asserting rather than assuming.
//
// The older two are the point of this test. A payout is a public record, and a
// tool that can only check the most recent one is not much of a check.
func TestBundledReportsVerify(t *testing.T) {
	for _, path := range []string{"example-v3.csv", "example-v2.csv", "example.csv", "example-legacy.csv"} {
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
					FeeRateDecimal:        parsed.header.feeRateDecimal,
					MinFeeRateSatVb:       parsed.header.minFeeRateSatVb,
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

// A schema 2 report states its fee rate instead of letting it be derived, so
// what remains checkable is whether the payout obeyed the policy it published.
// These are the ways it could claim compliance it does not have.
func TestVersion2FeePolicyIsAudited(t *testing.T) {
	base := func() *parsedCSV {
		parsed, err := loadReport("example-v2.csv")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return parsed
	}

	t.Run("the bundled report is clean", func(t *testing.T) {
		if issues := checkFeePolicy(base()); len(issues) != 0 {
			t.Errorf("unexpected issues: %v", issues)
		}
	})

	t.Run("the two sections must agree on the rate", func(t *testing.T) {
		p := base()
		p.summary.feeRateSatVb = p.header.feeRateDecimal + 1
		if issues := checkFeePolicy(p); len(issues) == 0 {
			t.Error("accepted a report whose two fee rates disagree")
		}
	})

	t.Run("a rate under the declared floor is caught", func(t *testing.T) {
		p := base()
		p.header.feeRateDecimal = 0.292
		p.summary.feeRateSatVb = 0.292
		if issues := checkFeePolicy(p); len(issues) == 0 {
			t.Error("accepted a rate below the floor the report itself declares")
		}
	})

	t.Run("claiming the floor without being at it is caught", func(t *testing.T) {
		p := base()
		p.header.feeRateSource = "floor"
		if issues := checkFeePolicy(p); len(issues) == 0 {
			t.Error("accepted a report claiming the floor applied when it did not")
		}
	})

	t.Run("a fee over the declared cap is caught", func(t *testing.T) {
		p := base()
		p.summary.estimatedMinerFeeSatoshi = p.header.poolSatoshi // 100% of the pool
		if issues := checkFeePolicy(p); len(issues) == 0 {
			t.Error("accepted a miner fee far over the cap the report declares")
		}
	})

	t.Run("version 1 reports are not held to version 2 rules", func(t *testing.T) {
		parsed, err := loadReport("example.csv")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if issues := checkFeePolicy(parsed); len(issues) != 0 {
			t.Errorf("a version 1 report was audited against version 2 policy: %v", issues)
		}
	})
}

// Version 3 split the settlement block in two: one ending the scoring window
// and one, six blocks later, supplying the seed. The split exists so that
// whoever mines the seed block cannot see which balances are being weighted -
// those were fixed six blocks earlier.
func TestVersion3CarriesBothSettlementBlocks(t *testing.T) {
	parsed, err := loadReport("example-v3.csv")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if parsed.header.schemaVersion != 3 {
		t.Fatalf("schema version = %d, want 3", parsed.header.schemaVersion)
	}
	if parsed.header.scoreBlockHeight == 0 || parsed.header.seedBlockHeight == 0 {
		t.Fatalf("v3 report parsed without both heights: score=%d seed=%d",
			parsed.header.scoreBlockHeight, parsed.header.seedBlockHeight)
	}
	if got := parsed.header.seedBlockHeight - parsed.header.scoreBlockHeight; got != 6 {
		t.Errorf("seed and score are %d blocks apart, want 6", got)
	}
	// The single-block field must stay empty. A v3 file that also set it would
	// be claiming both shapes at once.
	if parsed.header.settlementBlockHeight != 0 {
		t.Errorf("v3 report also carries settlement_block_height = %d",
			parsed.header.settlementBlockHeight)
	}
}

// Older reports must keep the shape they were written with. A payout is a
// public record; a tool that can only check the most recent one is not much of
// a check.
func TestOlderReportsKeepTheSingleSettlementBlock(t *testing.T) {
	for _, path := range []string{"example-v2.csv", "example.csv"} {
		t.Run(path, func(t *testing.T) {
			parsed, err := loadReport(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if parsed.header.settlementBlockHeight == 0 {
				t.Error("lost the settlement height this format carries")
			}
			if parsed.header.seedBlockHeight != 0 || parsed.header.scoreBlockHeight != 0 {
				t.Errorf("read version 3 columns out of a version %d report",
					parsed.header.schemaVersion)
			}
		})
	}
}

// Tampering with a recorded amount has to make the check fail - that is what
// "you can verify it yourself" means. The draw and the split are replayed from
// the file, so a figure that was edited afterwards no longer follows from them.
func TestATamperedWinnerAmountFailsVerification(t *testing.T) {
	parsed := loadTampered(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "result_winner,") {
			return line, false
		}
		f := strings.Split(line, ",")
		f[6] = "999999999" // final_reward_satoshi
		return strings.Join(f, ","), true
	})

	result, err := service.NewLotteryService().Compute(lotteryContextFrom(parsed))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if issues := compareResults(parsed, result); len(issues) == 0 {
		t.Error("an edited payout amount verified clean")
	}
}

// Substituting the seed in THIS report does not fail, and the tool is right.
//
// The bundled fixture draws 4 winners from 5 participants whose smallest score
// is a millionth of the others, so the small holder is never drawn and the same
// four come out under every seed - measured across 2000 of them. The amounts
// are then split in proportion to score, which the seed does not touch. The
// report is genuinely consistent with either seed, and saying "passed" is the
// correct answer to the question this tool asks.
//
// What it means is that no single report can demonstrate the seed matters.
// TestTheSeedDecidesTheDraw does that, with a fixture built for it: 2 winners
// from 5 participants within one order of magnitude, where 400 seeds produce 10
// different winner sets.
//
// This test exists so nobody reads the passing result above as "seeds are not
// checked" and goes looking for a bug that is not there.
func TestSubstitutingTheSeedOnThisFixtureIsNotDetectable(t *testing.T) {
	parsed := loadTampered(t, func(line string) (string, bool) {
		const seed = "0000000000000000000123abcdeadbeef"
		if !strings.Contains(line, seed) {
			return line, false
		}
		return strings.Replace(line, seed, "ffffffffffffffffffffffffffffffff", 1), true
	})

	result, err := service.NewLotteryService().Compute(lotteryContextFrom(parsed))
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if issues := compareResults(parsed, result); len(issues) != 0 {
		t.Errorf("this fixture became seed-sensitive; the note above is now wrong: %v", issues)
	}
}

// loadTampered rewrites one line of the v3 example and loads the result. It
// fails the test if nothing matched, so a fixture change cannot quietly turn a
// tampering test into a test of the untouched file.
func loadTampered(t *testing.T, edit func(string) (string, bool)) *parsedCSV {
	t.Helper()

	raw, err := os.ReadFile("example-v3.csv")
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if changed {
			break
		}
		if out, did := edit(line); did {
			lines[i], changed = out, true
		}
	}
	if !changed {
		t.Fatal("nothing in the fixture matched the edit; this test is no longer testing what it says")
	}

	path := filepath.Join(t.TempDir(), "tampered.csv")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	parsed, err := loadReport(path)
	if err != nil {
		t.Fatalf("a tampered file should still parse, so the mismatch is what reports it: %v", err)
	}
	return parsed
}
