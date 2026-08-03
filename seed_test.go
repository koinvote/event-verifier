package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/koinvote/event-verifier/service"
)

// The whole promise of this tool is that the draw was not ours to choose: the
// seed comes from a block hash nobody controlled at the time, and anyone can
// replay the draw from it. That promise is worth exactly as much as the seed's
// influence on the outcome - and nothing in the suite was checking it.
//
// TestBundledReportsVerify recomputes each bundled report from its recorded
// seed and compares. That looks like it covers this, and does not. Its fixture
// draws 4 winners from 5 participants where the smallest score is a millionth
// of the others, so the small holder is never drawn and the excluded one is
// always the same. Measured across 2000 seeds: one winner set, every time.
//
// So an implementation that ignored the seed entirely and returned the top k by
// score would pass every other test in this file. These two tests fail it.

// seedSensitiveContext draws 2 of 5 from scores within one order of magnitude,
// where which addresses come out genuinely depends on the seed.
func seedSensitiveContext(seed string) service.LotteryContext {
	return service.LotteryContext{
		Seed:        seed,
		PoolSatoshi: 100000000,
		WinnerCount: 2,
		Balances: []service.BalanceEntry{
			{Address: "bc1qaaa", Score: 302400000000},
			{Address: "bc1qbbb", Score: 201600000000},
			{Address: "bc1qccc", Score: 151200000000},
			{Address: "bc1qddd", Score: 100800000000},
			{Address: "bc1qeee", Score: 75600000000},
		},
		Params: service.LotteryParams{
			PlatformFeePercentage: 10,
			DustThresholdSatoshi:  600,
			// Version 2 path: the decimal rate is used and FeeRateSatVb stays
			// zero, matching how a node-priced report is verified.
			FeeRateDecimal:  1.191,
			MinFeeRateSatVb: 1,
			TxOverheadVBytes:      10,
			InputP2WSHBytes:       109,
			OutputDefaultVBytes:   31,
		},
	}
}

func winnerSet(t *testing.T, seed string) string {
	t.Helper()
	result, err := service.NewLotteryService().Compute(seedSensitiveContext(seed))
	if err != nil {
		t.Fatalf("seed %s: %v", seed, err)
	}
	addresses := make([]string, 0, len(result.Winners))
	for _, w := range result.Winners {
		addresses = append(addresses, w.Address)
	}
	sort.Strings(addresses)
	return strings.Join(addresses, ",")
}

// TestTheSeedDecidesTheDraw is the one that would catch a seed-blind
// implementation. Ranking by score would give the same two addresses forever.
func TestTheSeedDecidesTheDraw(t *testing.T) {
	sets := map[string]int{}
	for i := 0; i < 400; i++ {
		sets[winnerSet(t, fmt.Sprintf("%064x", i))]++
	}

	if len(sets) < 2 {
		t.Fatalf("400 seeds produced %d distinct winner set(s) - the draw is not using the seed", len(sets))
	}
	t.Logf("400 seeds produced %d distinct winner sets", len(sets))

	// Weighting still has to be visible in the outcome, or the draw is uniform
	// and a bigger holding buys nothing. The largest holder should appear in
	// noticeably more draws than the smallest.
	var withLargest, withSmallest int
	for set, n := range sets {
		if strings.Contains(set, "bc1qaaa") {
			withLargest += n
		}
		if strings.Contains(set, "bc1qeee") {
			withSmallest += n
		}
	}
	t.Logf("largest holder drawn %d/400, smallest %d/400", withLargest, withSmallest)
	if withLargest <= withSmallest {
		t.Errorf("largest holder drawn %d times vs smallest %d - score is not weighting the draw",
			withLargest, withSmallest)
	}
}

// TestTheSameSeedAlwaysDrawsTheSame is the other half. A verifier that produced
// a different answer on each run would be unable to contradict anything, and
// the failure would look like the payout being wrong rather than the tool being
// broken.
func TestTheSameSeedAlwaysDrawsTheSame(t *testing.T) {
	const seed = "0000000000000000000123abcdeadbeef"

	first := winnerSet(t, seed)
	for i := 0; i < 50; i++ {
		if got := winnerSet(t, seed); got != first {
			t.Fatalf("run %d drew %q, first run drew %q - the draw is not deterministic", i, got, first)
		}
	}
	t.Logf("50 runs of seed %s all drew %s", seed, first)
}
