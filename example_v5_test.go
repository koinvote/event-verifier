package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/koinvote/event-verifier/service"
)

// example-v5.csv is generated, not hand-written, so that its figures are the
// allocation's own and so that the fixture cannot drift from the code that
// checks it. Regenerate with UPDATE_EXAMPLE_V5=1 go test -run TestWriteExampleV5
// whenever the format or the algorithm changes; TestExampleV5IsUpToDate fails
// until the committed file matches.
//
// The participants are eight close scores: with 2000 units every remainder is
// in play, so changing the seed changes who receives the sampled units. That
// is the property the older fixtures lack, and the one a verifier most needs
// to demonstrate - that it would notice a substituted seed.

const exampleV5Path = "example-v5.csv"

func exampleV5Context() service.UnitsContext {
	scores := []int64{100, 110, 120, 130, 140, 150, 160, 170}
	balances := make([]service.BalanceEntry, len(scores))
	for i, s := range scores {
		balances[i] = service.BalanceEntry{Address: fmt.Sprintf("bc1q%04d", i), Score: s}
	}
	return service.UnitsContext{
		Public: service.UnitsPublicInputs{
			Seed:                        "ffffffffffffffffffffffffffffffff",
			PoolSatoshi:                 100_000_000,
			PlatformFeeBps:              1000,
			UnitFloorSatoshi:            600,
			MinUnits:                    8,
			OutputCap:                   2000,
			FeeBudgetPercent:            5,
			MaxFeePercent:               10,
			WaivedMaxFeePercent:         25,
			TxOverheadVBytes:            12,
			InputVBytes:                 110,
			OutputVBytes:                34,
			OpReturnVBytes:              840,
			SeedBlockMedianFeeRateSatVb: 1,
			Balances:                    balances,
		},
		Reported: service.UnitsPlatformReported{
			FeeRateMilliSatPerVb: 1191,
			FeeRateSource:        "node_estimate",
			FeeCapWaived:         false,
			LadderRateSource:     service.UnitsLadderRateSeedBlock,
			OpReturnVariant:      service.UnitsOpReturnFull,
		},
	}
}

// renderExampleV5 writes a version 5 report in the layout the backend
// publishes: one column-name row before each section, every participant
// listed under result_winner.
func renderExampleV5(ctx service.UnitsContext, result *service.UnitsResult) string {
	p := ctx.Public
	rep := ctx.Reported
	var b strings.Builder

	b.WriteString("type,schema_version,event_id,payout_txid,seed,score_block_height,score_block_hash,seed_block_height,seed_block_mtp,payout_block_height,payout_block_mtp," +
		"pool_satoshi,winner_count,platform_fee_percentage,dust_threshold_satoshi,fee_rate_sat_vb,fee_rate_milli_sat_vb,fee_rate_source,fee_target_blocks,min_fee_rate_sat_vb," +
		"max_fee_percentage,waived_max_fee_percentage,fee_cap_waived,tx_overhead_vbytes,input_p2wsh_bytes,output_default_vbytes,op_return_vbytes,op_return_variant," +
		"distribution_algorithm,unit_floor_satoshi,min_units,output_cap,fee_budget_percentage,seed_block_median_fee_rate_sat_vb,ladder_rate_source," +
		"unit_step_satoshi,unit_satoshi,unit_count,extra_units,max_outputs,draw_offset,residual_satoshi,residual_address\n")
	fmt.Fprintf(&b, "header,5,01KEXAMPLEV5UNITSALLOCATION,b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef01,%s,900100,0000000000000000000456def0011223344556677889900aabbccddeeff001122,900106,1794000000,900112,1794003600,"+
		"%d,0,%s,%d,%s,%d,%s,6,1,%d,%d,%t,%d,%d,%d,%d,%s,"+
		"%s,%d,%d,%d,%d,%d,%s,"+
		"%d,%d,%d,%d,%d,%s,%d,%s\n",
		p.Seed,
		p.PoolSatoshi, strconv.FormatFloat(float64(p.PlatformFeeBps)/100, 'f', -1, 64), p.UnitFloorSatoshi,
		strconv.FormatFloat(float64(rep.FeeRateMilliSatPerVb)/1000, 'f', -1, 64), rep.FeeRateMilliSatPerVb, rep.FeeRateSource,
		p.MaxFeePercent, p.WaivedMaxFeePercent, rep.FeeCapWaived, p.TxOverheadVBytes, p.InputVBytes, p.OutputVBytes, p.OpReturnVBytes, rep.OpReturnVariant,
		service.UnitsAlgorithmID, p.UnitFloorSatoshi, p.MinUnits, p.OutputCap, p.FeeBudgetPercent, p.SeedBlockMedianFeeRateSatVb, rep.LadderRateSource,
		result.UnitStepSatoshi, result.UnitSatoshi, result.UnitCount, result.ExtraUnits, result.MaxOutputs, result.DrawOffset, result.ResidualSatoshi, result.ResidualAddress)

	b.WriteString("type,address,referral_code,holding_score_sat_blocks,average_holding_satoshi,join_block_height\n")
	for _, row := range result.Rows {
		fmt.Fprintf(&b, "balance,%s,,%d,,\n", row.Address, row.Score)
	}

	b.WriteString("type,platform_fee_satoshi,estimated_miner_fee_satoshi,distributable_satoshi,fee_rate_sat_vb\n")
	fmt.Fprintf(&b, "result_summary,%d,%d,%d,%s\n", result.PlatformFeeSatoshi, result.MinerFeeSatoshi, result.DistributableSatoshi,
		strconv.FormatFloat(float64(rep.FeeRateMilliSatPerVb)/1000, 'f', -1, 64))

	b.WriteString("type,address,holding_score_sat_blocks,average_holding_satoshi,join_block_height,entitlement_satoshi,units_guaranteed,units_extra,original_reward_satoshi,final_reward_satoshi,is_dust\n")
	for _, row := range result.Rows {
		fmt.Fprintf(&b, "result_winner,%s,%d,,,%d,%d,%d,%d,%d,false\n",
			row.Address, row.Score, row.EntitlementSatoshi, row.UnitsGuaranteed, row.UnitsExtra, row.FinalRewardSatoshi, row.FinalRewardSatoshi)
	}
	return b.String()
}

func TestWriteExampleV5(t *testing.T) {
	if os.Getenv("UPDATE_EXAMPLE_V5") == "" {
		t.Skip("set UPDATE_EXAMPLE_V5=1 to regenerate example-v5.csv")
	}
	ctx := exampleV5Context()
	result, err := service.ComputeUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exampleV5Path, []byte(renderExampleV5(ctx, result)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExampleV5IsUpToDate(t *testing.T) {
	committed, err := os.ReadFile(exampleV5Path)
	if err != nil {
		t.Fatalf("read %s (regenerate with UPDATE_EXAMPLE_V5=1): %v", exampleV5Path, err)
	}
	ctx := exampleV5Context()
	result, err := service.ComputeUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != renderExampleV5(ctx, result) {
		t.Fatalf("%s does not match the generator; regenerate it with UPDATE_EXAMPLE_V5=1", exampleV5Path)
	}
}

func TestVersion5ReportVerifies(t *testing.T) {
	parsed, err := loadReport(exampleV5Path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if parsed.header.schemaVersion != 5 || parsed.header.distributionAlgorithm != service.UnitsAlgorithmID {
		t.Fatalf("schema %d algorithm %q", parsed.header.schemaVersion, parsed.header.distributionAlgorithm)
	}
	issues, err := verifyReport(parsed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, issue := range issues {
		t.Errorf("unexpected mismatch: %s", issue)
	}
	if len(parsed.winners) != len(parsed.balances) {
		t.Fatalf("a units_v2 report lists every participant: %d rows for %d balances", len(parsed.winners), len(parsed.balances))
	}
}

// loadTamperedV5 is loadTampered over the version 5 fixture.
func loadTamperedV5(t *testing.T, edit func(string) (string, bool)) (*parsedCSV, error) {
	t.Helper()
	raw, err := os.ReadFile(exampleV5Path)
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
	path := filepath.Join(t.TempDir(), "tampered-v5.csv")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return loadReport(path)
}

func mustVerifyIssues(t *testing.T, parsed *parsedCSV, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("a tampered file should still parse, so the mismatch is what reports it: %v", err)
	}
	issues, err := verifyReport(parsed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return issues
}

func TestVersion5TamperedAmountFails(t *testing.T) {
	parsed, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "result_winner,bc1q0003,") {
			return line, false
		}
		fields := strings.Split(line, ",")
		final, _ := strconv.ParseInt(fields[9], 10, 64)
		fields[9] = strconv.FormatInt(final+1, 10)
		return strings.Join(fields, ","), true
	})
	issues := mustVerifyIssues(t, parsed, err)
	if len(issues) == 0 {
		t.Fatal("a final reward raised by one satoshi still verified")
	}
}

// The version 5 fixture is built so that the seed matters. Substituting it
// must change the sampled units and fail the report - the check the older
// fixtures could never make.
func TestVersion5SubstitutedSeedIsDetected(t *testing.T) {
	parsed, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "header,5,") {
			return line, false
		}
		return strings.Replace(line, ",ffffffffffffffffffffffffffffffff,", ",0000000000000000000000000000beef,", 1), true
	})
	issues := mustVerifyIssues(t, parsed, err)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "draw_offset") || strings.Contains(issue, "units_extra") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a substituted seed was not detected; issues: %v", issues)
	}
}

func TestVersion5RequiresTheAlgorithmColumn(t *testing.T) {
	raw, err := os.ReadFile(exampleV5Path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.Replace(string(raw), "distribution_algorithm,", "distribution_algorithm_was_here,", 1)
	path := filepath.Join(t.TempDir(), "v5-no-algorithm.csv")
	if err := os.WriteFile(path, []byte(trimmed), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReport(path); err == nil || !strings.Contains(err.Error(), "distribution_algorithm") {
		t.Fatalf("a version 5 report without distribution_algorithm should be refused by name, got %v", err)
	}
}

func TestVersion5UnknownAlgorithmIsRefused(t *testing.T) {
	_, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "header,5,") {
			return line, false
		}
		return strings.Replace(line, ",units_v2,", ",units_v9,", 1), true
	})
	if err == nil || !strings.Contains(err.Error(), "units_v9") {
		t.Fatalf("an unknown algorithm should be refused by name, got %v", err)
	}
}

// A waived cap is only legitimate after a 72-hour wait, and the two block
// times that prove it are in the report. Claiming the waiver with a payout
// block too soon after the seed block must fail.
func TestVersion5WaiverRequiresTheWait(t *testing.T) {
	parsed, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "header,5,") {
			return line, false
		}
		return strings.Replace(line, ",false,12,110,34,", ",true,12,110,34,", 1), true
	})
	issues := mustVerifyIssues(t, parsed, err)
	if !parsed.header.feeCapWaived {
		t.Fatal("the edit did not set fee_cap_waived")
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "72") || strings.Contains(issue, "seconds after the seed block") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a waiver 3600 seconds after the seed block was accepted; issues: %v", issues)
	}
}

func TestVersion5UnknownLadderSourceIsFlagged(t *testing.T) {
	parsed, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "header,5,") {
			return line, false
		}
		return strings.Replace(line, ",seed_block_median,", ",operator_choice,", 1), true
	})
	issues := mustVerifyIssues(t, parsed, err)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "ladder_rate_source") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unknown ladder_rate_source was accepted; issues: %v", issues)
	}
}

// The seed block's fee rate sizes the unit. Claiming a different rate than
// the one the ladder was walked with changes the rung, which the recomputation
// catches.
func TestVersion5LadderIsRecomputedFromTheSeedBlockRate(t *testing.T) {
	parsed, err := loadTamperedV5(t, func(line string) (string, bool) {
		if !strings.HasPrefix(line, "header,5,") {
			return line, false
		}
		// seed_block_median_fee_rate_sat_vb is the value right before
		// ladder_rate_source. With a 1 BTC pool and eight addresses only an
		// absurd rate moves the rung, which is the point: the rung is a
		// function of this figure and nothing else.
		return strings.Replace(line, ",1,seed_block_median,", ",200000,seed_block_median,", 1), true
	})
	issues := mustVerifyIssues(t, parsed, err)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "unit_step_satoshi") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a changed seed-block rate did not move the unit step; issues: %v", issues)
	}
}
