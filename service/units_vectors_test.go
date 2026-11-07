package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Cross-repo vectors for the units_v2 allocation.
//
// The event-verifier carries a verbatim copy of lottery_units.go, and nothing
// but these vectors keeps the two copies computing the same thing. The
// backend writes the file with RISKSCAN_UNITS_VECTORS=<path>; the committed
// copy under testdata/ is checked against a fresh computation here, and the
// verifier holds the same bytes and does the same. A divergence shows up as a
// diff in either repository before it shows up as an unverifiable payout.
//
// The struct field order is the JSON field order, so the two repositories'
// copies of these types must match field for field for the files to be
// byte-equal.

type unitsVectorRow struct {
	Address     string `json:"address"`
	Score       int64  `json:"score"`
	Entitlement int64  `json:"entitlement_satoshi"`
	Guaranteed  int64  `json:"units_guaranteed"`
	Extra       int64  `json:"units_extra"`
	Final       int64  `json:"final_reward_satoshi"`
}

type unitsVectorExpected struct {
	Error           string           `json:"error,omitempty"`
	PlatformFee     int64            `json:"platform_fee_satoshi"`
	MinerFee        int64            `json:"miner_fee_satoshi"`
	Distributable   int64            `json:"distributable_satoshi"`
	UnitStep        int64            `json:"unit_step_satoshi"`
	Unit            int64            `json:"unit_satoshi"`
	UnitCount       int64            `json:"unit_count"`
	ExtraUnits      int64            `json:"extra_units"`
	MaxOutputs      int64            `json:"max_outputs"`
	TotalScore      string           `json:"total_score"`
	DrawOffset      string           `json:"draw_offset"`
	Residual        int64            `json:"residual_satoshi"`
	ResidualAddress string           `json:"residual_address"`
	Rows            []unitsVectorRow `json:"rows"`
}

type unitsVectorCase struct {
	Name                        string   `json:"name"`
	Seed                        string   `json:"seed"`
	Addresses                   []string `json:"addresses"`
	Scores                      []int64  `json:"scores"`
	PoolSatoshi                 int64    `json:"pool_satoshi"`
	PlatformFeeBps              int64    `json:"platform_fee_bps"`
	UnitFloorSatoshi            int64    `json:"unit_floor_satoshi"`
	MinUnits                    int64    `json:"min_units"`
	OutputCap                   int64    `json:"output_cap"`
	FeeBudgetPercent            int64    `json:"fee_budget_percent"`
	MaxFeePercent               int64    `json:"max_fee_percent"`
	WaivedMaxFeePercent         int64    `json:"waived_max_fee_percent"`
	TxOverheadVBytes            int64    `json:"tx_overhead_vbytes"`
	InputVBytes                 int64    `json:"input_vbytes"`
	OutputVBytes                int64    `json:"output_vbytes"`
	OpReturnVBytes              int64    `json:"op_return_vbytes"`
	SeedBlockMedianFeeRateSatVb int64    `json:"seed_block_median_fee_rate_sat_vb"`
	FeeRateMilliSatPerVb        int64    `json:"fee_rate_milli_sat_per_vb"`
	FeeCapWaived                bool     `json:"fee_cap_waived"`

	Expected *unitsVectorExpected `json:"expected,omitempty"`
}

func (c unitsVectorCase) context() UnitsContext {
	balances := make([]BalanceEntry, len(c.Addresses))
	for i := range c.Addresses {
		balances[i] = BalanceEntry{Address: c.Addresses[i], Score: c.Scores[i]}
	}
	return UnitsContext{
		Public: UnitsPublicInputs{
			Seed:                        c.Seed,
			PoolSatoshi:                 c.PoolSatoshi,
			PlatformFeeBps:              c.PlatformFeeBps,
			UnitFloorSatoshi:            c.UnitFloorSatoshi,
			MinUnits:                    c.MinUnits,
			OutputCap:                   c.OutputCap,
			FeeBudgetPercent:            c.FeeBudgetPercent,
			MaxFeePercent:               c.MaxFeePercent,
			WaivedMaxFeePercent:         c.WaivedMaxFeePercent,
			TxOverheadVBytes:            c.TxOverheadVBytes,
			InputVBytes:                 c.InputVBytes,
			OutputVBytes:                c.OutputVBytes,
			OpReturnVBytes:              c.OpReturnVBytes,
			SeedBlockMedianFeeRateSatVb: c.SeedBlockMedianFeeRateSatVb,
			Balances:                    balances,
		},
		Reported: UnitsPlatformReported{
			FeeRateMilliSatPerVb: c.FeeRateMilliSatPerVb,
			FeeRateSource:        "node_estimate",
			FeeCapWaived:         c.FeeCapWaived,
			LadderRateSource:     UnitsLadderRateSeedBlock,
			OpReturnVariant:      UnitsOpReturnFull,
		},
	}
}

func unitsVectorExpectedFrom(result *UnitsResult, err error) *unitsVectorExpected {
	if err != nil {
		var feeErr *UnitsFeeError
		var dustErr *UnitsDustError
		switch {
		case errors.As(err, &feeErr):
			return &unitsVectorExpected{Error: fmt.Sprintf("fee_over_cap:%d:%t", feeErr.LimitPercent, feeErr.Waived)}
		case errors.As(err, &dustErr):
			return &unitsVectorExpected{Error: "unit_below_dust"}
		default:
			return &unitsVectorExpected{Error: err.Error()}
		}
	}
	out := &unitsVectorExpected{
		PlatformFee:     result.PlatformFeeSatoshi,
		MinerFee:        result.MinerFeeSatoshi,
		Distributable:   result.DistributableSatoshi,
		UnitStep:        result.UnitStepSatoshi,
		Unit:            result.UnitSatoshi,
		UnitCount:       result.UnitCount,
		ExtraUnits:      result.ExtraUnits,
		MaxOutputs:      result.MaxOutputs,
		TotalScore:      result.TotalScore,
		DrawOffset:      result.DrawOffset,
		Residual:        result.ResidualSatoshi,
		ResidualAddress: result.ResidualAddress,
		Rows:            make([]unitsVectorRow, 0, len(result.Rows)),
	}
	for _, row := range result.Rows {
		out.Rows = append(out.Rows, unitsVectorRow{
			Address: row.Address, Score: row.Score, Entitlement: row.EntitlementSatoshi,
			Guaranteed: row.UnitsGuaranteed, Extra: row.UnitsExtra, Final: row.FinalRewardSatoshi,
		})
	}
	return out
}

func riskScanUnitsCases() []unitsVectorCase {
	mk := func(name, seed string, scores []int64, pool int64) unitsVectorCase {
		addrs := make([]string, len(scores))
		for i := range scores {
			addrs[i] = fmt.Sprintf("bc1q%04d", i)
		}
		return unitsVectorCase{
			Name: name, Seed: seed, Addresses: addrs, Scores: scores, PoolSatoshi: pool,
			PlatformFeeBps: 1000, UnitFloorSatoshi: 600, MinUnits: 8, OutputCap: 2000,
			FeeBudgetPercent: 5, MaxFeePercent: 10, WaivedMaxFeePercent: 25,
			TxOverheadVBytes: 12, InputVBytes: 110, OutputVBytes: 34, OpReturnVBytes: 0,
			SeedBlockMedianFeeRateSatVb: 1, FeeRateMilliSatPerVb: 1191,
		}
	}
	equal := func(n int, score int64) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = score
		}
		return out
	}

	cases := []unitsVectorCase{
		mk("close scores, 5 addresses", "0000000000000000000123abcdeadbeef",
			[]int64{302400000000, 201600000000, 151200000000, 100800000000, 75600000000}, 100000000),
		mk("close scores, 8 addresses", "ffffffffffffffffffffffffffffffff",
			[]int64{100, 110, 120, 130, 140, 150, 160, 170}, 100000000),
		mk("all equal, 10 addresses", "1111111111111111111111111111abcd", equal(10, 1000), 100000000),
		mk("99.9 percent one holder", "0000000000000000000000000000beef",
			[]int64{999_000_000, 1_000_000 / 6, 1_000_000 / 6, 1_000_000 / 6, 1_000_000 / 6, 1_000_000 / 6, 1_000_000 / 6}, 100000000),
		mk("long tail", "abcdefabcdefabcdefabcdefabcdef00", []int64{500_000_000, 7, 6, 5, 4, 3, 2, 1}, 100000000),
		mk("btc-time magnitudes", "0000000000000000000123abcdeadbeef",
			[]int64{302400000000, 100800000000, 75600000000, 30240000000}, 100000000),
		mk("single participant", "0000000000000000000000000000000c", []int64{9}, 100000000),
		mk("terms example 1", "0000000000000000000123abcdeadbeef", []int64{2000, 1000, 500}, 10_000_000),
		mk("terms example 2", "00000000000000000000456def0abc9e2", []int64{2000, 1000, 70, 500}, 40_000),
		mk("minimum pool, 100 equal, 8 units", "0000000000000000000000000000000d", equal(100, 1_000_000), 50_000),
		mk("output cap 2000 reached", "0000000000000000000000000000000e",
			[]int64{5_000_000_000_000_000, 5_000_000_000_000_000, 5_000_000_000_000_000}, 100_000_000),
	}

	// Ladder and policy edges.
	c := mk("ladder: 21 addresses at 5 sat/vB -> 6000", "0000000000000000000000000000000f", equal(21, 1_000_000), 50_000)
	c.SeedBlockMedianFeeRateSatVb = 5
	cases = append(cases, c)

	c = mk("min units pinned at high seed rate", "00000000000000000000000000000010", equal(5, 100), 50_000)
	c.SeedBlockMedianFeeRateSatVb = 50
	cases = append(cases, c)

	c = mk("fee over the 10 percent cap", "00000000000000000000000000000011", equal(8, 1000), 50_000)
	c.SeedBlockMedianFeeRateSatVb = 100
	c.FeeRateMilliSatPerVb = 12_000
	cases = append(cases, c)

	c = mk("same fee, cap waived", "00000000000000000000000000000011", equal(8, 1000), 50_000)
	c.SeedBlockMedianFeeRateSatVb = 100
	c.FeeRateMilliSatPerVb = 12_000
	c.FeeCapWaived = true
	cases = append(cases, c)

	c = mk("fee over the 25 percent waived cap", "00000000000000000000000000000011", equal(8, 1000), 50_000)
	c.SeedBlockMedianFeeRateSatVb = 100
	c.FeeRateMilliSatPerVb = 30_000
	c.FeeCapWaived = true
	cases = append(cases, c)

	c = mk("unit below dust, refused even when waived", "00000000000000000000000000000012", equal(66, 1000), 50_000)
	c.FeeRateMilliSatPerVb = 4_000
	c.FeeCapWaived = true
	cases = append(cases, c)

	c = mk("op_return summary reserved in the fee", "00000000000000000000000000000013", equal(6, 7), 200_000)
	c.OpReturnVBytes = 840
	cases = append(cases, c)

	// Vary the seed over allocations that are seed-sensitive by
	// construction, so a boundary change has many chances to surface: the
	// eight close scores (2000 units, every remainder in play) and the
	// hundred equal holders (eight sampled units, nothing guaranteed).
	eight := cases[1]
	for i := 0; i < 40; i++ {
		v := eight
		v.Name = fmt.Sprintf("close scores, 8 addresses, seed %02d", i)
		v.Seed = fmt.Sprintf("%064x", i)
		cases = append(cases, v)
	}
	hundred := cases[9]
	for i := 0; i < 8; i++ {
		v := hundred
		v.Name = fmt.Sprintf("minimum pool, 100 equal, 8 units, seed %02d", i)
		v.Seed = fmt.Sprintf("%064x", 1000+i)
		cases = append(cases, v)
	}
	return cases
}

func riskScanUnitsRun(cases []unitsVectorCase) []unitsVectorCase {
	out := make([]unitsVectorCase, 0, len(cases))
	for _, c := range cases {
		c.Expected = unitsVectorExpectedFrom(ComputeUnits(c.context()))
		out = append(out, c)
	}
	return out
}

func unitsVectorsJSON(cases []unitsVectorCase) []byte {
	blob, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(blob, '\n')
}

const unitsVectorsFixture = "testdata/units_vectors.json"

func TestRiskScanEmitUnitsVectors(t *testing.T) {
	path := os.Getenv("RISKSCAN_UNITS_VECTORS")
	if path == "" {
		t.Skip("set RISKSCAN_UNITS_VECTORS=<path> to write the cross-repo units vectors")
	}
	blob := unitsVectorsJSON(riskScanUnitsRun(riskScanUnitsCases()))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d units vectors to %s", len(riskScanUnitsCases()), path)
}

// TestRiskScanUnitsVectorsMatchTheCommittedFile recomputes every committed
// vector. The verifier runs the same check over the same file, so the two
// repositories can only drift apart by failing a test on one side.
func TestRiskScanUnitsVectorsMatchTheCommittedFile(t *testing.T) {
	committed, err := os.ReadFile(unitsVectorsFixture)
	if err != nil {
		t.Fatalf("read %s (regenerate with RISKSCAN_UNITS_VECTORS): %v", unitsVectorsFixture, err)
	}
	var cases []unitsVectorCase
	if err := json.Unmarshal(committed, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		c.Expected = nil
	}
	fresh := unitsVectorsJSON(riskScanUnitsRun(cases))
	if string(fresh) != string(committed) {
		t.Fatalf("%s no longer matches a fresh computation; regenerate it with RISKSCAN_UNITS_VECTORS and copy it to the verifier", unitsVectorsFixture)
	}
	if len(cases) < 50 {
		t.Fatalf("only %d vectors", len(cases))
	}
}

func TestRiskScanUnitsVectorsAreSeedSensitive(t *testing.T) {
	seen := map[string]int{}
	base := riskScanUnitsCases()[1]
	for i := 0; i < 200; i++ {
		c := base
		c.Seed = fmt.Sprintf("%064x", i)
		result, err := ComputeUnits(c.context())
		if err != nil {
			t.Fatal(err)
		}
		seen[fmt.Sprint(result.PaidRows())]++
	}
	if len(seen) < 2 {
		t.Fatalf("200 seeds produced %d distinct allocation(s)", len(seen))
	}
}
