package service

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// unitsTestContext builds a context with the production defaults decided for
// units_v2: 600 sat floor, 8 units minimum, 2000 outputs cap, 5% fee budget,
// 10% / 25% caps, 12 + 110 + 34 vbytes, 1 sat/vB seed-block rate and a
// 1.191 sat/vB paid rate.
func unitsTestContext(seed string, pool int64, balances []BalanceEntry) UnitsContext {
	return UnitsContext{
		Public: UnitsPublicInputs{
			Seed:                        seed,
			PoolSatoshi:                 pool,
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
			OpReturnVBytes:              0,
			SeedBlockMedianFeeRateSatVb: 1,
			Balances:                    balances,
		},
		Reported: UnitsPlatformReported{
			FeeRateMilliSatPerVb: 1191,
			FeeRateSource:        "node_estimate",
			LadderRateSource:     UnitsLadderRateSeedBlock,
			OpReturnVariant:      UnitsOpReturnFull,
		},
	}
}

func unitsAddresses(scores []int64) []BalanceEntry {
	out := make([]BalanceEntry, len(scores))
	for i, s := range scores {
		out[i] = BalanceEntry{Address: fmt.Sprintf("bc1q%04d", i), Score: s}
	}
	return out
}

func unitsSeed(i int) string { return fmt.Sprintf("%064x", i) }

func mustComputeUnits(t *testing.T, ctx UnitsContext) *UnitsResult {
	t.Helper()
	result, err := ComputeUnits(ctx)
	if err != nil {
		t.Fatalf("ComputeUnits: %v", err)
	}
	return result
}

// The terms' worked examples and the report's synthetic scenarios, used by
// the Monte Carlo test and by the trace that feeds the published numbers.
var unitsExample1 = unitsTestContext("0000000000000000000123abcdeadbeef", 10_000_000, []BalanceEntry{
	{Address: "A", Score: 2000}, {Address: "B", Score: 1000}, {Address: "C", Score: 500},
})

var unitsExample2 = unitsTestContext("00000000000000000000456def0abc9e2", 40_000, []BalanceEntry{
	{Address: "A", Score: 2000}, {Address: "B", Score: 1000}, {Address: "C", Score: 70}, {Address: "D", Score: 500},
})

func unitsSyntheticScenarios() map[string]UnitsContext {
	rng := rand.New(rand.NewSource(7))
	lognormal := func(n int) []int64 {
		scores := make([]int64, n)
		for i := range scores {
			scores[i] = int64(math.Exp(rng.NormFloat64()*1.5) * 1e6)
			if scores[i] <= 0 {
				scores[i] = 1
			}
		}
		return scores
	}
	whale := make([]int64, 31)
	whale[0] = 30_000_000
	for i := 1; i < 31; i++ {
		whale[i] = 1_000_000
	}
	return map[string]UnitsContext{
		"example 1":               unitsExample1,
		"example 2":               unitsExample2,
		"one whale and 30 small":  unitsTestContext("", 50_000, unitsAddresses(whale)),
		"20 lognormal addresses":  unitsTestContext("", 50_000, unitsAddresses(lognormal(20))),
		"100 lognormal addresses": unitsTestContext("", 1_000_000, unitsAddresses(lognormal(100))),
		"100 equal, 8 units":      unitsTestContext("", 50_000, unitsAddresses(repeatScore(100, 1_000_000))),
		"btc-time magnitudes, 4":  unitsTestContext("", 100_000_000, unitsAddresses([]int64{302400000000, 100800000000, 75600000000, 30240000000})),
	}
}

func repeatScore(n int, score int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = score
	}
	return out
}

func sumUnitsFinal(r *UnitsResult) int64 {
	var total int64
	for _, row := range r.Rows {
		total += row.FinalRewardSatoshi
	}
	return total
}

// ---------------------------------------------------------------------------
// Invariants over random inputs.

func unitsRandomContext(rng *rand.Rand) UnitsContext {
	n := 1 + rng.Intn(60)
	scores := make([]int64, n)
	for i := range scores {
		// Spread over nine orders of magnitude so tiny holders sit beside
		// whales, which is where rounding and the residual rule get exercised.
		scores[i] = int64(math.Pow(10, 4+rng.Float64()*9))
	}
	pools := []int64{50_000, 100_000, 250_000, 1_000_000, 10_000_000, 100_000_000}
	ctx := unitsTestContext(unitsSeed(rng.Intn(1_000_000)), pools[rng.Intn(len(pools))], unitsAddresses(scores))
	ctx.Public.SeedBlockMedianFeeRateSatVb = int64(1 + rng.Intn(20))
	ctx.Reported.FeeRateMilliSatPerVb = int64(1000 + rng.Intn(5000))
	ctx.Public.OpReturnVBytes = int64(rng.Intn(900))
	return ctx
}

func TestUnitsConservesThePool(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	checked := 0
	for i := 0; i < 2000; i++ {
		ctx := unitsRandomContext(rng)
		result, err := ComputeUnits(ctx)
		var feeErr *UnitsFeeError
		var dustErr *UnitsDustError
		if errors.As(err, &feeErr) || errors.As(err, &dustErr) {
			continue // a deferral, not an allocation
		}
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		checked++
		if got := sumUnitsFinal(result); got != result.DistributableSatoshi {
			t.Fatalf("case %d: paid %d but distributable is %d", i, got, result.DistributableSatoshi)
		}
		if total := result.PlatformFeeSatoshi + result.MinerFeeSatoshi + result.DistributableSatoshi; total != ctx.Public.PoolSatoshi {
			t.Fatalf("case %d: platform %d + miner %d + distributable %d != pool %d",
				i, result.PlatformFeeSatoshi, result.MinerFeeSatoshi, result.DistributableSatoshi, ctx.Public.PoolSatoshi)
		}
		if paid := int64(len(result.PaidRows())); result.ResidualSatoshi < 0 || result.ResidualSatoshi >= paid {
			t.Fatalf("case %d: residual %d is not under the %d addresses paid", i, result.ResidualSatoshi, paid)
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d of 2000 random cases produced an allocation; the generator is mostly hitting the fee cap", checked)
	}
}

func TestUnitsPaysWithinOneUnitOfEntitlement(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 1000; i++ {
		ctx := unitsRandomContext(rng)
		result, err := ComputeUnits(ctx)
		if err != nil {
			continue
		}
		for _, row := range result.Rows {
			diff := row.FinalRewardSatoshi - row.EntitlementSatoshi
			if diff < 0 {
				diff = -diff
			}
			// Both figures are T times a share of K, each rounded down once,
			// so they differ by under one unit plus the two roundings (plus
			// the residual for the address that receives it).
			bound := result.UnitSatoshi + 2
			if row.Address == result.ResidualAddress {
				bound += result.ResidualSatoshi
			}
			if diff >= bound {
				t.Fatalf("case %d: %s paid %d against entitlement %d, off by %d which is not under %d",
					i, row.Address, row.FinalRewardSatoshi, row.EntitlementSatoshi, diff, bound)
			}
		}
	}
}

func TestUnitsOutputsNeverExceedTheBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 1000; i++ {
		ctx := unitsRandomContext(rng)
		result, err := ComputeUnits(ctx)
		if err != nil {
			continue
		}
		paid := int64(len(result.PaidRows()))
		n := int64(len(result.Rows))
		if paid > result.MaxOutputs {
			t.Fatalf("case %d: %d outputs but the fee was reserved for %d", i, paid, result.MaxOutputs)
		}
		if result.MaxOutputs > n || result.MaxOutputs > result.UnitCount || result.MaxOutputs > ctx.Public.OutputCap {
			t.Fatalf("case %d: max outputs %d, units %d, participants %d, cap %d", i, result.MaxOutputs, result.UnitCount, n, ctx.Public.OutputCap)
		}
		if n > ctx.Public.OutputCap && result.UnitCount > ctx.Public.OutputCap {
			t.Fatalf("case %d: %d participants but %d units, over the cap", i, n, result.UnitCount)
		}
		var extra int64
		for _, row := range result.Rows {
			if row.UnitsExtra > 1 {
				t.Fatalf("case %d: %s was sampled %d times", i, row.Address, row.UnitsExtra)
			}
			extra += row.UnitsExtra
		}
		if extra != result.ExtraUnits {
			t.Fatalf("case %d: sampled %d units but %d were to be sampled", i, extra, result.ExtraUnits)
		}
	}
}

func TestUnitsIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for i := 0; i < 200; i++ {
		ctx := unitsRandomContext(rng)
		first, err1 := ComputeUnits(ctx)
		second, err2 := ComputeUnits(ctx)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("case %d: errors differ: %v vs %v", i, err1, err2)
		}
		if err1 != nil {
			continue
		}
		if fmt.Sprint(first) != fmt.Sprint(second) {
			t.Fatalf("case %d: two runs of the same input differ", i)
		}
	}
}

func TestUnitsInputOrderDoesNotMatter(t *testing.T) {
	ctx := unitsSyntheticScenarios()["20 lognormal addresses"]
	ctx.Public.Seed = unitsSeed(5)
	reversed := make([]BalanceEntry, len(ctx.Public.Balances))
	for i, b := range ctx.Public.Balances {
		reversed[len(reversed)-1-i] = b
	}
	shuffled := ctx
	shuffled.Public.Balances = reversed
	if fmt.Sprint(mustComputeUnits(t, ctx)) != fmt.Sprint(mustComputeUnits(t, shuffled)) {
		t.Fatal("the allocation depends on the order the balances were supplied in")
	}
}

func TestUnitsSeedChangesTheAllocation(t *testing.T) {
	base := unitsSyntheticScenarios()["100 equal, 8 units"]
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		ctx := base
		ctx.Public.Seed = unitsSeed(i)
		result := mustComputeUnits(t, ctx)
		key := fmt.Sprint(result.PaidRows())
		seen[key]++
	}
	if len(seen) < 2 {
		t.Fatalf("200 seeds produced %d distinct allocation(s)", len(seen))
	}
	t.Logf("200 seeds produced %d distinct allocations", len(seen))
}

func TestUnitsSingleParticipantTakesEverything(t *testing.T) {
	ctx := unitsTestContext(unitsSeed(1), 100_000, []BalanceEntry{{Address: "only", Score: 12345}})
	result := mustComputeUnits(t, ctx)
	if len(result.Rows) != 1 || result.Rows[0].FinalRewardSatoshi != result.DistributableSatoshi {
		t.Fatalf("single participant got %+v of %d", result.Rows, result.DistributableSatoshi)
	}
	if result.DrawOffset != "" {
		t.Fatalf("nothing should be sampled with one participant, offset %q", result.DrawOffset)
	}
}

func TestUnitsNoSamplingWhenRemaindersAreZero(t *testing.T) {
	// Four equal scores and a pool small enough that the minimum of eight
	// units applies: 8 * score / (4 * score) is exactly 2 with no remainder.
	ctx := unitsTestContext(unitsSeed(9), 50_000, unitsAddresses(repeatScore(4, 777)))
	ctx.Public.SeedBlockMedianFeeRateSatVb = 100 // forces the ladder up and K down to the minimum
	result := mustComputeUnits(t, ctx)
	if result.UnitCount != 8 || result.ExtraUnits != 0 || result.DrawOffset != "" {
		t.Fatalf("expected 8 units, none sampled: %+v", result)
	}
	for _, row := range result.Rows {
		if row.UnitsGuaranteed != 2 || row.UnitsExtra != 0 {
			t.Fatalf("expected 2 guaranteed units each: %+v", row)
		}
	}
}

func TestUnitsResidualNeverCreatesAnOutputOfItsOwn(t *testing.T) {
	// 100 equal addresses with 8 units: nobody has a guaranteed unit, the
	// eight are sampled, and the tie-break address (the first) is usually not
	// among them. The residual must then go to someone who is.
	base := unitsSyntheticScenarios()["100 equal, 8 units"]
	seenUnpaidFirst := false
	for i := 0; i < 100; i++ {
		ctx := base
		ctx.Public.Seed = unitsSeed(i)
		result := mustComputeUnits(t, ctx)
		if result.ResidualSatoshi == 0 {
			continue
		}
		paid := map[string]int64{}
		for _, row := range result.PaidRows() {
			paid[row.Address] = row.FinalRewardSatoshi
		}
		amount, ok := paid[result.ResidualAddress]
		if !ok {
			t.Fatalf("seed %d: residual went to %s, which is not paid", i, result.ResidualAddress)
		}
		if amount < result.UnitSatoshi {
			t.Fatalf("seed %d: residual recipient %s holds only %d", i, result.ResidualAddress, amount)
		}
		if _, firstPaid := paid[result.Rows[0].Address]; !firstPaid {
			seenUnpaidFirst = true
		}
	}
	if !seenUnpaidFirst {
		t.Fatal("no seed left the tie-break address unpaid; the test did not exercise the rule")
	}
}

func TestUnitsResidualTieGoesToTheFirstAddress(t *testing.T) {
	ctx := unitsTestContext(unitsSeed(3), 50_000, []BalanceEntry{
		{Address: "bbb", Score: 5000}, {Address: "aaa", Score: 5000}, {Address: "ccc", Score: 10},
	})
	result := mustComputeUnits(t, ctx)
	if result.ResidualSatoshi == 0 {
		t.Skip("no residual with these inputs")
	}
	if result.ResidualAddress != "aaa" {
		t.Fatalf("residual went to %s, expected the first of the tied top scores", result.ResidualAddress)
	}
}

func TestUnitsLadderTable(t *testing.T) {
	// Pool 50,000 with a 10% platform fee leaves T0 = 45,000 and a 5% budget
	// of 2,250 sats for the outputs' own fee at 34 vB each.
	cases := []struct {
		participants int
		seedRate     int64
		wantStep     int64
	}{
		{21, 1, 600},
		{21, 2, 600},
		{21, 5, 6000},
		{10, 2, 600},
	}
	for _, c := range cases {
		ctx := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses(repeatScore(c.participants, 1_000_000)))
		ctx.Public.SeedBlockMedianFeeRateSatVb = c.seedRate
		result := mustComputeUnits(t, ctx)
		if result.UnitStepSatoshi != c.wantStep {
			t.Fatalf("%d participants at %d sat/vB: unit step %d, expected %d", c.participants, c.seedRate, result.UnitStepSatoshi, c.wantStep)
		}
	}
}

func TestUnitsLadderSkipsRungsBelowTheFloor(t *testing.T) {
	ctx := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses(repeatScore(5, 100)))
	ctx.Public.UnitFloorSatoshi = 2000
	result := mustComputeUnits(t, ctx)
	if result.UnitStepSatoshi != 3000 {
		t.Fatalf("floor 2000 should land on the 3000 rung, got %d", result.UnitStepSatoshi)
	}
}

func TestUnitsMinUnitsPinsTheCount(t *testing.T) {
	ctx := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses(repeatScore(5, 100)))
	ctx.Public.SeedBlockMedianFeeRateSatVb = 50 // every rung's outputs blow the budget until T0/u <= 1
	result := mustComputeUnits(t, ctx)
	if result.UnitCount != 8 {
		t.Fatalf("expected the minimum of 8 units, got %d (step %d)", result.UnitCount, result.UnitStepSatoshi)
	}
	if result.UnitSatoshi != result.DistributableSatoshi/8 {
		t.Fatalf("unit %d is not distributable/8 = %d", result.UnitSatoshi, result.DistributableSatoshi/8)
	}
}

func TestUnitsUnitIsAtLeastTheRungWhileTheFeeIsWithinCap(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	for i := 0; i < 1000; i++ {
		ctx := unitsRandomContext(rng)
		result, err := ComputeUnits(ctx)
		if err != nil {
			continue
		}
		if result.UnitCount == ctx.Public.MinUnits {
			continue // pinned by the minimum, the rung no longer bounds the unit
		}
		if result.UnitSatoshi < result.UnitStepSatoshi {
			t.Fatalf("case %d: unit %d fell below its rung %d with fee %d of pool %d",
				i, result.UnitSatoshi, result.UnitStepSatoshi, result.MinerFeeSatoshi, ctx.Public.PoolSatoshi)
		}
	}
}

func TestUnitsBigIntMagnitudes(t *testing.T) {
	// (90,000,000 - 10,000,000) / 600 = 133,333 units times a 5e15 score is
	// 6.7e20, far past int64. Three such holders must still be paid exactly
	// and in proportion.
	ctx := unitsTestContext(unitsSeed(1), 100_000_000, unitsAddresses([]int64{5_000_000_000_000_000, 5_000_000_000_000_000, 5_000_000_000_000_000}))
	result := mustComputeUnits(t, ctx)
	if result.UnitCount != 133_333 {
		t.Fatalf("expected 133,333 units, got %d", result.UnitCount)
	}
	if got := sumUnitsFinal(result); got != result.DistributableSatoshi {
		t.Fatalf("paid %d of %d", got, result.DistributableSatoshi)
	}
	for _, row := range result.Rows {
		if row.UnitsGuaranteed != 44_444 {
			t.Fatalf("each holder should have 44,444 guaranteed units: %+v", row)
		}
	}
}

func TestUnitsOutputCapBindsOnlyWithMoreParticipantsThanIt(t *testing.T) {
	// Three participants, a pool that would cut into 13,333 units: the cap
	// must not touch it. 2,500 participants: the cap must.
	few := unitsTestContext(unitsSeed(1), 10_000_000, unitsAddresses([]int64{2000, 1000, 500}))
	if result := mustComputeUnits(t, few); result.UnitCount != 13_333 {
		t.Fatalf("3 participants: expected 13,333 units, got %d", result.UnitCount)
	}
	many := unitsTestContext(unitsSeed(1), 100_000_000, unitsAddresses(repeatScore(2500, 1_000_000)))
	result := mustComputeUnits(t, many)
	if result.UnitCount != 2000 || result.MaxOutputs > 2000 {
		t.Fatalf("2,500 participants: expected 2,000 units and at most 2,000 outputs, got %d units, %d outputs", result.UnitCount, result.MaxOutputs)
	}
}

func TestUnitsFeePolicy(t *testing.T) {
	// 50,000 pool: the 10% cap is 5,000 sats, the waived cap 12,500. With
	// eight outputs the transaction is 12+110+34*9 = 428 vB, so 12 sat/vB is
	// 5,136 (over the cap) and 30 sat/vB is 12,840 (over the waived cap).
	base := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses(repeatScore(8, 1000)))
	base.Public.SeedBlockMedianFeeRateSatVb = 100

	over := base
	over.Reported.FeeRateMilliSatPerVb = 12_000
	var feeErr *UnitsFeeError
	if _, err := ComputeUnits(over); !errors.As(err, &feeErr) || feeErr.Waived || feeErr.LimitPercent != 10 {
		t.Fatalf("12 sat/vB should exceed the 10%% cap, got %v", err)
	}

	waived := over
	waived.Reported.FeeCapWaived = true
	if _, err := ComputeUnits(waived); err != nil {
		t.Fatalf("the same fee should pass once waived: %v", err)
	}

	farOver := waived
	farOver.Reported.FeeRateMilliSatPerVb = 30_000
	if _, err := ComputeUnits(farOver); !errors.As(err, &feeErr) || !feeErr.Waived || feeErr.LimitPercent != 25 {
		t.Fatalf("30 sat/vB should exceed the 25%% waived cap, got %v", err)
	}
}

func TestUnitsDustGuardRefusesEvenWhenWaived(t *testing.T) {
	// 66 units from (45,000 - 5,000) / 600, so the unit drops under 546 once
	// the fee passes 45,000 - 66*546 = 8,964 sats. Only the waived cap
	// (12,500) admits such a fee, and the guard must still refuse it.
	ctx := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses(repeatScore(66, 1000)))
	ctx.Reported.FeeCapWaived = true
	ctx.Reported.FeeRateMilliSatPerVb = 4_000 // 12+110+34*67 = 2,400 vB -> 9,600 sats
	var dustErr *UnitsDustError
	if _, err := ComputeUnits(ctx); !errors.As(err, &dustErr) {
		t.Fatalf("expected a dust refusal, got %v", err)
	}
	if dustErr.UnitSatoshi >= UnitsDustLimitSatoshi {
		t.Fatalf("dust error for a unit of %d", dustErr.UnitSatoshi)
	}
}

func TestUnitsRejectsBadInputs(t *testing.T) {
	good := unitsTestContext(unitsSeed(1), 50_000, unitsAddresses([]int64{1, 2, 3}))
	cases := map[string]func(*UnitsContext){
		"empty seed":          func(c *UnitsContext) { c.Public.Seed = "" },
		"zero pool":           func(c *UnitsContext) { c.Public.PoolSatoshi = 0 },
		"fee of 100%":         func(c *UnitsContext) { c.Public.PlatformFeeBps = 10000 },
		"floor under dust":    func(c *UnitsContext) { c.Public.UnitFloorSatoshi = 500 },
		"zero min units":      func(c *UnitsContext) { c.Public.MinUnits = 0 },
		"cap under min units": func(c *UnitsContext) { c.Public.OutputCap = 7 },
		"max fee 0":           func(c *UnitsContext) { c.Public.MaxFeePercent = 0 },
		"waived cap below":    func(c *UnitsContext) { c.Public.WaivedMaxFeePercent = 5 },
		"zero output vbytes":  func(c *UnitsContext) { c.Public.OutputVBytes = 0 },
		"zero paid rate":      func(c *UnitsContext) { c.Reported.FeeRateMilliSatPerVb = 0 },
	}
	for name, mutate := range cases {
		ctx := good
		mutate(&ctx)
		if _, err := ComputeUnits(ctx); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	empty := good
	empty.Public.Balances = []BalanceEntry{{Address: "x", Score: 0}}
	if _, err := ComputeUnits(empty); !errors.Is(err, ErrUnitsNoParticipants) {
		t.Errorf("no positive scores: got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Monte Carlo: the expected payout is the score share.

const unitsMonteCarloSeeds = 20_000

// unitsMeanShares runs a scenario over the Monte Carlo seeds and returns each
// address's mean paid share of the distributable pool.
func unitsMeanShares(t *testing.T, base UnitsContext) map[string]float64 {
	t.Helper()
	sums := map[string]float64{}
	for i := 0; i < unitsMonteCarloSeeds; i++ {
		ctx := base
		ctx.Public.Seed = unitsSeed(i)
		result := mustComputeUnits(t, ctx)
		for _, row := range result.Rows {
			sums[row.Address] += float64(row.FinalRewardSatoshi) / float64(result.DistributableSatoshi)
		}
	}
	for address := range sums {
		sums[address] /= unitsMonteCarloSeeds
	}
	return sums
}

func unitsScoreShares(balances []BalanceEntry) map[string]float64 {
	var total float64
	for _, b := range balances {
		total += float64(b.Score)
	}
	shares := map[string]float64{}
	for _, b := range balances {
		shares[b.Address] += float64(b.Score) / total
	}
	return shares
}

func TestUnitsMonteCarloExpectedValueIsProportional(t *testing.T) {
	const tolerance = 0.005 // half a percentage point
	for name, base := range unitsSyntheticScenarios() {
		means := unitsMeanShares(t, base)
		shares := unitsScoreShares(base.Public.Balances)
		worst := 0.0
		worstAddress := ""
		for address, share := range shares {
			gap := math.Abs(means[address] - share)
			if gap > worst {
				worst, worstAddress = gap, address
			}
			if gap >= tolerance {
				t.Errorf("%s: %s mean paid share %.4f vs score share %.4f (gap %.4f)", name, address, means[address], share, gap)
			}
		}
		t.Logf("%-26s %3d addresses, %d seeds: worst gap %.4f pp at %s", name, len(shares), unitsMonteCarloSeeds, worst*100, worstAddress)
	}
}

func TestUnitsMonteCarloSplittingAnAddressKeepsItsExpectedTotal(t *testing.T) {
	const tolerance = 0.005
	base := unitsSyntheticScenarios()["20 lognormal addresses"]
	// Split the largest holder into a third and two thirds under two new
	// addresses that sort to opposite ends of the line.
	largest := 0
	for i, b := range base.Public.Balances {
		if b.Score > base.Public.Balances[largest].Score {
			largest = i
		}
	}
	whole := base.Public.Balances[largest]
	split := base
	split.Public.Balances = make([]BalanceEntry, 0, len(base.Public.Balances)+1)
	for i, b := range base.Public.Balances {
		if i == largest {
			split.Public.Balances = append(split.Public.Balances,
				BalanceEntry{Address: "aaaa-first", Score: whole.Score / 3},
				BalanceEntry{Address: "zzzz-last", Score: whole.Score - whole.Score/3})
			continue
		}
		split.Public.Balances = append(split.Public.Balances, b)
	}

	before := unitsMeanShares(t, base)
	after := unitsMeanShares(t, split)
	combined := after["aaaa-first"] + after["zzzz-last"]
	if gap := math.Abs(combined - before[whole.Address]); gap >= tolerance {
		t.Fatalf("splitting %s changed its expected share: whole %.4f vs parts %.4f + %.4f",
			whole.Address, before[whole.Address], after["aaaa-first"], after["zzzz-last"])
	}
	for _, b := range base.Public.Balances {
		if b.Address == whole.Address {
			continue
		}
		if gap := math.Abs(after[b.Address] - before[b.Address]); gap >= tolerance {
			t.Errorf("splitting another address moved %s from %.4f to %.4f", b.Address, before[b.Address], after[b.Address])
		}
	}
	t.Logf("whole %.4f, parts %.4f + %.4f = %.4f", before[whole.Address], after["aaaa-first"], after["zzzz-last"], combined)
}

// ---------------------------------------------------------------------------
// The step-by-step figures for the terms' worked examples. Run with -v to
// read them; the published examples are copied from this output, never
// worked by hand.

func TestUnitsExampleTraces(t *testing.T) {
	for _, example := range []struct {
		name string
		ctx  UnitsContext
	}{{"Example 1", unitsExample1}, {"Example 2", unitsExample2}} {
		result := mustComputeUnits(t, example.ctx)
		t.Log(unitsTrace(example.name, example.ctx, result))
	}
}

func unitsTrace(name string, ctx UnitsContext, r *UnitsResult) string {
	var b strings.Builder
	p := ctx.Public
	fmt.Fprintf(&b, "\n== %s ==\n", name)
	fmt.Fprintf(&b, "pool %d, platform %d bps, seed %s, seed-block rate %d sat/vB, paid rate %d msat/vB, sizes %d+%d+%d*outputs+%d vB\n",
		p.PoolSatoshi, p.PlatformFeeBps, p.Seed, p.SeedBlockMedianFeeRateSatVb, ctx.Reported.FeeRateMilliSatPerVb,
		p.TxOverheadVBytes, p.InputVBytes, p.OutputVBytes, p.OpReturnVBytes)
	fmt.Fprintf(&b, "sorted participants (W = %s):\n", r.TotalScore)
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "  %-4s score %d\n", row.Address, row.Score)
	}
	fmt.Fprintf(&b, "platform fee %d, T0 %d, cap fee %d\n", r.PlatformFeeSatoshi, p.PoolSatoshi-r.PlatformFeeSatoshi, p.PoolSatoshi*p.MaxFeePercent/100)
	fmt.Fprintf(&b, "unit step u %d, K %d, extra units m %d, max outputs %d\n", r.UnitStepSatoshi, r.UnitCount, r.ExtraUnits, r.MaxOutputs)
	fmt.Fprintf(&b, "miner fee %d, T %d, one unit = T/K = %d (rounded down), residual %d -> %s\n", r.MinerFeeSatoshi, r.DistributableSatoshi, r.UnitSatoshi, r.ResidualSatoshi, r.ResidualAddress)
	// The sampling line, rebuilt from the public inputs so the reader can
	// follow every point to the address it lands in.
	W, _ := new(big.Int).SetString(r.TotalScore, 10)
	K := big.NewInt(r.UnitCount)
	cursor := new(big.Int)
	fmt.Fprintf(&b, "remainders (K*score mod W) laid end to end:\n")
	for _, row := range r.Rows {
		rem := new(big.Int).Mod(new(big.Int).Mul(K, big.NewInt(row.Score)), W)
		end := new(big.Int).Add(cursor, rem)
		fmt.Fprintf(&b, "  %-4s rem %s  line [%s, %s)\n", row.Address, rem, cursor, end)
		cursor = end
	}
	if r.DrawOffset != "" {
		offset, _ := new(big.Int).SetString(r.DrawOffset, 10)
		points := make([]string, 0, r.ExtraUnits)
		for k := int64(0); k < r.ExtraUnits; k++ {
			points = append(points, new(big.Int).Add(offset, new(big.Int).Mul(W, big.NewInt(k))).String())
		}
		fmt.Fprintf(&b, "draw offset r = %s; points r + k*W = %s\n", r.DrawOffset, strings.Join(points, ", "))
	} else {
		fmt.Fprintf(&b, "no remainders to sample\n")
	}
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "  %-4s q %d  extra %d  entitlement %d  paid T*%d/K = %d\n",
			row.Address, row.UnitsGuaranteed, row.UnitsExtra, row.EntitlementSatoshi, row.UnitsGuaranteed+row.UnitsExtra, row.FinalRewardSatoshi)
	}
	return b.String()
}
