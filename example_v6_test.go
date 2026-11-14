package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/koinvote/event-verifier/service"
)

// example-v6.csv is the version 6 layout over the same inputs as
// example-v5.csv: identical arithmetic, so the two files differ only in the
// names version 6 changed. Regenerate with UPDATE_EXAMPLE_V6=1 go test -run
// TestWriteExampleV6; TestExampleV6IsUpToDate fails until the committed file
// matches.

const exampleV6Path = "example-v6.csv"

// renderExampleV6 writes a version 6 report in the layout the backend
// publishes: one column-name row before each section, every participant
// listed under result_participant.
func renderExampleV6(ctx service.UnitsContext, result *service.UnitsResult) string {
	p := ctx.Public
	rep := ctx.Reported
	var b strings.Builder

	b.WriteString("type,schema_version,event_id,payout_txid,seed,score_block_height,score_block_hash,seed_block_height,seed_block_mtp,payout_block_height,payout_block_mtp," +
		"pool_satoshi,participant_count,platform_fee_percentage,fee_rate_sat_vb,fee_rate_milli_sat_vb,fee_rate_source,fee_target_blocks,min_fee_rate_sat_vb," +
		"max_fee_percentage,waived_max_fee_percentage,fee_cap_waived,tx_overhead_vbytes,input_p2wsh_bytes,output_default_vbytes,op_return_vbytes,op_return_variant," +
		"distribution_algorithm,unit_floor_satoshi,min_units,output_cap,fee_budget_percentage,seed_block_median_fee_rate_sat_vb,ladder_rate_source," +
		"unit_step_satoshi,unit_satoshi,unit_count,extra_units,max_outputs,draw_offset,residual_satoshi,residual_address\n")
	fmt.Fprintf(&b, "header,6,01KEXAMPLEV6UNITSALLOCATION,b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef01,%s,900100,0000000000000000000456def0011223344556677889900aabbccddeeff001122,900106,1794000000,900112,1794003600,"+
		"%d,%d,%s,%s,%d,%s,6,1,%s,%s,%t,%d,%d,%d,%d,%s,"+
		"%s,%d,%d,%d,%s,%d,%s,"+
		"%d,%d,%d,%d,%d,%s,%d,%s\n",
		p.Seed,
		p.PoolSatoshi, len(result.Rows), strconv.FormatFloat(float64(p.PlatformFeeBps)/100, 'f', -1, 64),
		strconv.FormatFloat(float64(rep.FeeRateMilliSatPerVb)/1000, 'f', -1, 64), rep.FeeRateMilliSatPerVb, rep.FeeRateSource,
		pct(p.MaxFeeBps), pct(p.WaivedMaxFeeBps), rep.FeeCapWaived, p.TxOverheadVBytes, p.InputVBytes, p.OutputVBytes, p.OpReturnVBytes, rep.OpReturnVariant,
		service.UnitsAlgorithmID, p.UnitFloorSatoshi, p.MinUnits, p.OutputCap, pct(p.FeeBudgetBps), p.SeedBlockMedianFeeRateSatVb, rep.LadderRateSource,
		result.UnitStepSatoshi, result.UnitSatoshi, result.UnitCount, result.ExtraUnits, result.MaxOutputs, result.DrawOffset, result.ResidualSatoshi, result.ResidualAddress)

	b.WriteString("type,address,referral_code,holding_score_sat_blocks,average_holding_satoshi,join_block_height\n")
	for _, row := range result.Rows {
		fmt.Fprintf(&b, "balance,%s,,%d,,\n", row.Address, row.Score)
	}

	b.WriteString("type,platform_fee_satoshi,estimated_miner_fee_satoshi,distributable_satoshi,fee_rate_sat_vb\n")
	fmt.Fprintf(&b, "result_summary,%d,%d,%d,%s\n", result.PlatformFeeSatoshi, result.MinerFeeSatoshi, result.DistributableSatoshi,
		strconv.FormatFloat(float64(rep.FeeRateMilliSatPerVb)/1000, 'f', -1, 64))

	b.WriteString("type,address,holding_score_sat_blocks,average_holding_satoshi,join_block_height,entitlement_satoshi,units_guaranteed,units_extra,final_reward_satoshi\n")
	for _, row := range result.Rows {
		fmt.Fprintf(&b, "result_participant,%s,%d,,,%d,%d,%d,%d\n",
			row.Address, row.Score, row.EntitlementSatoshi, row.UnitsGuaranteed, row.UnitsExtra, row.FinalRewardSatoshi)
	}
	return b.String()
}

func TestWriteExampleV6(t *testing.T) {
	if os.Getenv("UPDATE_EXAMPLE_V6") == "" {
		t.Skip("set UPDATE_EXAMPLE_V6=1 to regenerate example-v6.csv")
	}
	ctx := exampleV5Context()
	result, err := service.ComputeUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exampleV6Path, []byte(renderExampleV6(ctx, result)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExampleV6IsUpToDate(t *testing.T) {
	committed, err := os.ReadFile(exampleV6Path)
	if err != nil {
		t.Fatalf("read %s (regenerate with UPDATE_EXAMPLE_V6=1): %v", exampleV6Path, err)
	}
	ctx := exampleV5Context()
	result, err := service.ComputeUnits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != renderExampleV6(ctx, result) {
		t.Fatalf("%s does not match the generator; regenerate it with UPDATE_EXAMPLE_V6=1", exampleV6Path)
	}
}

func TestVersion6ReportVerifies(t *testing.T) {
	parsed, err := loadReport(exampleV6Path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if parsed.header.schemaVersion != 6 || parsed.header.distributionAlgorithm != service.UnitsAlgorithmID {
		t.Fatalf("schema %d algorithm %q", parsed.header.schemaVersion, parsed.header.distributionAlgorithm)
	}
	if parsed.header.participantCount != len(parsed.balances) {
		t.Fatalf("participant_count %d for %d balances", parsed.header.participantCount, len(parsed.balances))
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

// A version 6 header that miscounts its participants is caught, not trusted.
func TestVersion6ParticipantCountIsChecked(t *testing.T) {
	parsed, err := loadReport(exampleV6Path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	parsed.header.participantCount++
	issues, err := verifyReport(parsed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	found := false
	for _, issue := range issues {
		if len(issue) >= len("participant_count mismatch") && issue[:len("participant_count mismatch")] == "participant_count mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a participant_count mismatch, got %q", issues)
	}
}

// exampleBackendV6Path is a schema 6 report written by the backend itself
// (koinvote/backend internal/jobs/testdata/payout_report_golden_v6.csv), with
// the transaction id and the payout block filled in the way the broadcast and
// confirm jobs do. When the backend regenerates its golden file, copy it here.
const exampleBackendV6Path = "example-backend-v6.csv"

func TestBackendWrittenVersion6ReportVerifies(t *testing.T) {
	parsed, err := loadReport(exampleBackendV6Path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if parsed.header.schemaVersion != 6 || parsed.header.distributionAlgorithm != service.UnitsAlgorithmID {
		t.Fatalf("schema %d algorithm %q", parsed.header.schemaVersion, parsed.header.distributionAlgorithm)
	}
	if parsed.header.payoutBlockMTP == 0 || parsed.header.seedBlockMTP == 0 {
		t.Fatal("the backend's report carries both block times; the fixture has lost one")
	}
	issues, err := verifyReport(parsed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, issue := range issues {
		t.Errorf("unexpected mismatch against the backend's report: %s", issue)
	}
	if len(parsed.winners) != len(parsed.balances) {
		t.Fatalf("a units_v2 report lists every participant: %d rows for %d balances", len(parsed.winners), len(parsed.balances))
	}
}
