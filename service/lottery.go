package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
)

var ErrNoParticipants = errors.New("no participants")

// LotteryService handles winner selection and payout planning (pure computation).
type LotteryService struct {
}

// NewLotteryService creates a LotteryService instance.
func NewLotteryService() *LotteryService {
	return &LotteryService{}
}

// Winner represents a selected address with weights.
type Winner struct {
	Address string
	Score   int64
}

// LotteryContext is the input for lottery computation (provided by job or verifier).
type LotteryContext struct {
	Seed        string
	PoolSatoshi int64
	WinnerCount int
	Balances    []BalanceEntry
	Params      LotteryParams
}

// BalanceEntry represents a participant's lottery weight.
type BalanceEntry struct {
	Address      string
	Score        int64
	ReferralCode *string
}

// LotteryParams holds the parameters needed for distribution calculation.
type LotteryParams struct {
	PlatformFeePercentage float64
	DustThresholdSatoshi  int64
	PayoutFeeMultiplier   float64

	// FeeRateSatVb is the version 1 rate: an integer, multiplied by
	// PayoutFeeMultiplier to get the rate the transaction offered.
	FeeRateSatVb int64

	// FeeRateDecimal is the version 2 rate, taken straight from the report.
	//
	// From schema version 2 the fee comes from the node's own estimator at the
	// moment of planning. That number cannot be recomputed afterwards - the
	// mempool it described is gone - so the report states it and this tool
	// checks that everything downstream follows from it, rather than deriving
	// it from a constant and a multiplier the way version 1 could.
	//
	// Zero means a version 1 report, and the integer path above is used.
	FeeRateDecimal float64
	// MinFeeRateSatVb is the floor the report declares. Checked, not applied:
	// if the stated rate is under it, the report contradicts its own policy.
	MinFeeRateSatVb float64

	TxOverheadVBytes    int
	InputP2WSHBytes     int
	OutputDefaultVBytes int
}

// LotteryResult represents payout calculation results.
type LotteryResult struct {
	PlatformFeeSatoshi       int64
	EstimatedMinerFeeSatoshi int64
	DistributableSatoshi     int64
	FeeRateSatVb             float64
	Winners                  []WinnerResult
}

// WinnerResult holds round results per winner.
type WinnerResult struct {
	Address               string
	Score                 int64
	OriginalRewardSatoshi int64
	IsDust                bool
	FinalRewardSatoshi    int64
}

// Compute performs winner selection and distribution based on input context.
func (s *LotteryService) Compute(ctx LotteryContext) (*LotteryResult, error) {
	if ctx.PoolSatoshi <= 0 {
		return nil, fmt.Errorf("poolSatoshi must be > 0")
	}
	if ctx.WinnerCount <= 0 {
		return nil, fmt.Errorf("winnerCount must be > 0")
	}
	if ctx.Seed == "" {
		return nil, fmt.Errorf("seed must not be empty")
	}

	participants := selectEligibleParticipants(ctx.Balances)
	if len(participants) == 0 {
		return nil, ErrNoParticipants
	}

	winnerCount := ctx.WinnerCount
	if winnerCount > len(participants) {
		winnerCount = len(participants)
	}

	winners, err := s.computeWinners(ctx.Seed, participants, winnerCount)
	if err != nil {
		return nil, err
	}

	result, err := s.planDistribution(ctx.PoolSatoshi, winners, ctx.Params)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func (s *LotteryService) computeWinners(seed string, participants []BalanceEntry, winnerCount int) ([]Winner, error) {
	if winnerCount <= 0 {
		return nil, fmt.Errorf("winnerCount must be > 0")
	}
	if winnerCount > len(participants) {
		winnerCount = len(participants)
	}

	// Ensure deterministic ordering so the same seed yields the same winners.
	sort.Slice(participants, func(i, j int) bool { return participants[i].Address < participants[j].Address })

	// If everyone wins, skip random sampling.
	if winnerCount == len(participants) {
		winners := make([]Winner, 0, len(participants))
		for _, p := range participants {
			winners = append(winners, Winner{Address: p.Address, Score: p.Score})
		}
		return winners, nil
	}

	// Copy candidates so we can remove winners without mutating input.
	pool := make([]BalanceEntry, len(participants))
	copy(pool, participants)

	// Deterministic RNG ensures reproducible draws for the same seed.
	rng := newDetRNG(seed)
	winners := make([]Winner, 0, winnerCount)

	for draw := 0; draw < winnerCount; draw++ {
		// Recompute total weight for the remaining pool.
		total := big.NewInt(0)
		for _, e := range pool {
			total.Add(total, big.NewInt(e.Score))
		}
		if total.Sign() <= 0 {
			return nil, fmt.Errorf("remaining total score is zero")
		}

		// Draw a random value in [0, total) and select by cumulative weights.
		r := rng.randBig(total)
		acc := big.NewInt(0)
		idx := -1
		for i, e := range pool {
			acc.Add(acc, big.NewInt(e.Score))
			if acc.Cmp(r) > 0 {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, fmt.Errorf("failed to select winner")
		}

		// Record the winner and remove them from the pool to avoid duplicates.
		winners = append(winners, Winner{Address: pool[idx].Address, Score: pool[idx].Score})
		last := len(pool) - 1
		pool[idx] = pool[last]
		pool = pool[:last]
	}

	return winners, nil
}

func (s *LotteryService) planDistribution(poolSat int64, winners []Winner, params LotteryParams) (*LotteryResult, error) {
	if poolSat <= 0 || len(winners) == 0 {
		return nil, fmt.Errorf("invalid inputs")
	}

	// 1) Calculate platform fee.
	platformFee := int64(0)
	if params.PlatformFeePercentage > 0 {
		// Convert percentage to basis points (2 decimals) before integer math to avoid float drift.
		bps := int64(math.Round(params.PlatformFeePercentage * 100))
		platformFee = poolSat * bps / 10000
		if platformFee > poolSat {
			platformFee = poolSat
		}
	}

	minDust := params.DustThresholdSatoshi
	if minDust <= 0 {
		minDust = 10_000
	}

	// Include a platform fee output when platform fee is non-zero.
	platformOutput := 0
	if platformFee > 0 {
		platformOutput = 1
	}

	// 2) Round 1: estimate miner fee with full winner set and compute provisional rewards.
	feeRate := effectiveFeeRate(params)
	fee1 := estimateMinerFee(len(winners)+platformOutput, feeRate, params)
	distributable1 := poolSat - platformFee - fee1
	if distributable1 <= 0 {
		return nil, fmt.Errorf("distributable amount is non-positive")
	}

	originalRewards := splitProportional(distributable1, winners)
	results := make([]WinnerResult, 0, len(winners))
	kept := make([]Winner, 0, len(winners))
	for i, w := range winners {
		original := originalRewards[i]
		isDust := original < minDust
		results = append(results, WinnerResult{
			Address:               w.Address,
			Score:                 w.Score,
			OriginalRewardSatoshi: original,
			IsDust:                isDust,
		})
		if !isDust {
			kept = append(kept, w)
		}
	}

	if len(kept) == 0 {
		// 3) Fallback: if all dust, pick the largest score as the sole payout target.
		// Fallback: pick the largest score as the only payout target.
		sort.Slice(winners, func(i, j int) bool { return winners[i].Score > winners[j].Score })
		fee2 := estimateMinerFee(1+platformOutput, feeRate, params)
		distributable2 := poolSat - platformFee - fee2
		if distributable2 < 0 {
			distributable2 = 0
		}

		targetAddress := winners[0].Address
		for i := range results {
			if results[i].Address == targetAddress {
				results[i].IsDust = false
				results[i].FinalRewardSatoshi = distributable2
			} else {
				results[i].FinalRewardSatoshi = 0
			}
		}

		return &LotteryResult{
			PlatformFeeSatoshi:       platformFee,
			EstimatedMinerFeeSatoshi: fee2,
			DistributableSatoshi:     distributable2,
			FeeRateSatVb:             feeRate,
			Winners:                  results,
		}, nil
	}

	// 4) Round 2: re-estimate miner fee with non-dust winners and recompute payouts.
	fee2 := estimateMinerFee(len(kept)+platformOutput, feeRate, params)
	distributable2 := poolSat - platformFee - fee2
	if distributable2 <= 0 {
		return nil, fmt.Errorf("distributable amount is non-positive")
	}

	finalRewards := splitProportional(distributable2, kept)
	finalRewards = applyResidual(finalRewards, distributable2)

	// 5) Fill final rewards back to the full winner list (dust winners stay at 0).
	finalIdx := 0
	for i := range results {
		if results[i].IsDust {
			continue
		}
		results[i].FinalRewardSatoshi = finalRewards[finalIdx]
		finalIdx++
	}

	return &LotteryResult{
		PlatformFeeSatoshi:       platformFee,
		EstimatedMinerFeeSatoshi: fee2,
		DistributableSatoshi:     distributable2,
		FeeRateSatVb:             feeRate,
		Winners:                  results,
	}, nil
}

// effectiveFeeRate returns the rate the transaction offered, in sat/vB.
//
// Version 2 reports state it outright; version 1 reports state the two numbers
// it was derived from. Both are returned as a decimal so the rest of the
// arithmetic is shared - for a version 1 report the value is a whole number
// either way, so nothing about how those files verify changes.
func effectiveFeeRate(params LotteryParams) float64 {
	if params.FeeRateDecimal > 0 {
		return params.FeeRateDecimal
	}
	multiplier := params.PayoutFeeMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return float64(int64(float64(params.FeeRateSatVb) * multiplier))
}

// estimateMinerFee rounds up, which for a version 1 report is a no-op: those
// rates are whole numbers, so rate x vbytes is already an integer and the old
// files verify to exactly the same figures as before.
func estimateMinerFee(outputs int, feeRate float64, params LotteryParams) int64 {
	if outputs <= 0 {
		return 0
	}
	overhead := params.TxOverheadVBytes
	if overhead <= 0 {
		overhead = 10
	}
	inP2WSH := params.InputP2WSHBytes
	if inP2WSH <= 0 {
		inP2WSH = 109
	}
	outDefault := params.OutputDefaultVBytes
	if outDefault <= 0 {
		outDefault = 31
	}

	vbytes := overhead + inP2WSH + outDefault*outputs
	return int64(math.Ceil(feeRate * float64(vbytes)))
}

func splitProportional(total int64, winners []Winner) []int64 {
	out := make([]int64, len(winners))
	if total <= 0 || len(winners) == 0 {
		return out
	}
	// big.Int because the multiplication below overflows int64 at ordinary
	// sizes. A BTC-Time score is satoshi x blocks held, so a 2 BTC holder in a
	// week-long event carries 2e11; multiplied by a 1 BTC pool's distributable
	// that is 1.8e19, past the int64 ceiling of 9.2e18.
	//
	// The failure was silent and picked the wrong winner: the product wrapped
	// negative, the largest holder's share came out below the dust threshold,
	// and they were dropped from the payout while a small holder took the lot.
	// computeWinners above has always used big.Int for the same reason; this
	// was the one place left doing the arithmetic in int64.
	sum := new(big.Int)
	for _, w := range winners {
		sum.Add(sum, big.NewInt(w.Score))
	}
	if sum.Sign() == 0 {
		each := total / int64(len(winners))
		for i := range out {
			out[i] = each
		}
		return out
	}
	// Each share is rounded down, so the shares sum to slightly less than total
	// — by under one satoshi per winner. Round two hands that remainder to the
	// largest share via applyResidual, so final amounts are exact; round one's
	// figures are only used to decide who is dust, where being a satoshi light
	// can only matter to a winner sitting exactly on the threshold.
	bigTotal := big.NewInt(total)
	for i, w := range winners {
		// share = total * score / sum, and score <= sum, so the result is at
		// most total and always fits back into int64.
		share := new(big.Int).Mul(bigTotal, big.NewInt(w.Score))
		out[i] = share.Div(share, sum).Int64()
	}
	return out
}

func applyResidual(rewards []int64, total int64) []int64 {
	var sum int64
	for _, v := range rewards {
		sum += v
	}
	residual := total - sum
	if residual <= 0 || len(rewards) == 0 {
		return rewards
	}

	maxIdx := 0
	for i := 1; i < len(rewards); i++ {
		if rewards[i] > rewards[maxIdx] {
			maxIdx = i
		}
	}
	rewards[maxIdx] += residual
	return rewards
}

// selectEligibleParticipants de-duplicates by address and filters to eligible participants.
func selectEligibleParticipants(balances []BalanceEntry) []BalanceEntry {
	agg := make(map[string]int64)
	for _, b := range balances {
		if b.Address == "" || b.Score <= 0 {
			continue
		}
		agg[b.Address] += b.Score
	}

	if len(agg) == 0 {
		return nil
	}

	out := make([]BalanceEntry, 0, len(agg))
	for address, score := range agg {
		out = append(out, BalanceEntry{Address: address, Score: score})
	}
	return out
}

type detRNG struct {
	seed    []byte
	counter uint64
	buf     []byte
	off     int
}

func newDetRNG(seed string) *detRNG {
	return &detRNG{seed: []byte(seed)}
}

func (r *detRNG) refill() {
	h := sha256.New()
	h.Write(r.seed)
	var c [8]byte
	for i := 0; i < 8; i++ {
		c[7-i] = byte(r.counter >> (8 * i))
	}
	h.Write(c[:])
	r.buf = h.Sum(nil)
	r.off = 0
	r.counter++
}

func (r *detRNG) read(n int) []byte {
	out := make([]byte, n)
	pos := 0
	for pos < n {
		if r.off >= len(r.buf) {
			r.refill()
		}
		k := copy(out[pos:], r.buf[r.off:])
		r.off += k
		pos += k
	}
	return out
}

func (r *detRNG) randBig(n *big.Int) *big.Int {
	if n.Sign() <= 0 {
		return big.NewInt(0)
	}
	bitLen := n.BitLen()
	byteLen := (bitLen + 7) / 8
	for {
		b := r.read(byteLen)
		x := new(big.Int).SetBytes(b)
		if x.Cmp(n) < 0 {
			return x
		}
	}
}
