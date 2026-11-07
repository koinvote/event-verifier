package service

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
)

// Proportional payout with unit rounding ("units_v2").
//
// The lottery in lottery.go draws K winners without replacement and then
// splits the pool among the winners only. Whenever there are more
// participants than winners, that pays large holders more than their share of
// the score and small holders less - the pool is divided by the winners'
// score, not everyone's, and the large holders are almost always drawn. The
// expected payout stops being proportional to the holding score, which is the
// one property the product promises.
//
// This file replaces the draw with rounding. Every address is entitled to
// T * w_i / W, where T is the pool after fees, w_i its score and W the total
// score. The pool is cut into K equal units; the whole units of each
// entitlement are paid outright ("guaranteed"), and the fractional parts are
// settled by systematic sampling: the fractions are laid end to end in
// address order, one random offset is taken from the seed, and a point every
// W along the line awards one extra unit to the address it lands in. An
// address's chance of the extra unit is exactly its fraction, so its expected
// payout is exactly its entitlement, and the sampling hands out exactly the
// units that remain. Each address is then paid T * units / K, rounded down
// once, so the pool is short by less than one satoshi per address paid.
// Outputs never exceed the participants, and when there are more
// participants than a transaction can carry, the unit count is capped so
// that outputs cannot exceed the cap either.
//
// Two groups of inputs, and the verifier trusts only the first:
//
//   - UnitsPublicInputs are facts anyone can establish from the chain and the
//     published report: the seed, the pool, the fee schedule, the scores, the
//     unit ladder, the output cap, and the median fee rate of the seed block.
//     Everything that decides who gets how many units derives from these and
//     nothing else. The verifier recomputes all of it.
//   - UnitsPlatformReported are the platform's own statements: the fee rate it
//     paid (its node's estimate, which no one can reproduce later) and the
//     flags it set. They scale the unit's value in satoshi uniformly for
//     everyone and decide whether a payout goes out now; they never change
//     the number of units an address receives. The verifier checks them
//     against policy (floor, caps, arithmetic) but does not feed them into
//     the allocation.
//
// This file depends only on BalanceEntry and detRNG from lottery.go so that
// it can be copied verbatim into the event-verifier repository, whose copy of
// lottery.go has a different LotteryParams. Both repositories must run this
// exact code: the published report is checked by recomputing it.
//
// All arithmetic is integer. Scores are satoshi x blocks and reach 1e13 in a
// week-long event; multiplied by K up to 2000 that is past int64, so every
// product with a score is big.Int. Fee rates arrive as a decimal from the
// node's estimator and are carried as milli-satoshi per vbyte so that the fee
// is an exact integer ceiling rather than a float rounded at the edge.

const (
	// UnitsAlgorithmID names this allocation in payout plans and verification
	// reports, so a verifier can pick the matching code path.
	UnitsAlgorithmID = "units_v2"

	// UnitsDustLimitSatoshi is the largest dust threshold Bitcoin Core applies
	// to the output types the platform pays (546 for P2PKH). An output below
	// it is refused by every node, and a refused payout is stuck for good, so
	// no unit may ever be worth less.
	UnitsDustLimitSatoshi int64 = 546

	// Ladder rate sources: where the fee rate that sized the unit came from.
	UnitsLadderRateSeedBlock    = "seed_block_median"
	UnitsLadderRateNodeEstimate = "node_estimate"

	// OP_RETURN variants the payout transaction may carry.
	UnitsOpReturnFull    = "full"
	UnitsOpReturnMinimal = "minimal"
)

// unitLadderBase is the first rung of the unit ladder; every rung doubles the
// one before (600, 1200, 2400, 4800, ...). The ladder is fixed so that a
// verifier can recompute which rung the platform had to land on from public
// inputs alone, and it is fine-grained so that the unit never jumps far past
// what the fee budget requires.
const unitLadderBase int64 = 600

// UnitLadderStep returns rung i of the unit ladder.
func UnitLadderStep(i int) int64 {
	return unitLadderBase << uint(i)
}

// UnitsPublicInputs are the inputs a verifier reproduces the allocation from.
// Every field is either on the chain, in the published report, or a constant
// of the published rules.
type UnitsPublicInputs struct {
	// Seed is the seed block's hash as a hex string. It is fed to detRNG as
	// the string's bytes, exactly as the lottery does.
	Seed string
	// PoolSatoshi is the prize pool before any deduction.
	PoolSatoshi int64
	// PlatformFeeBps is the platform fee in basis points (1000 = 10%).
	PlatformFeeBps int64
	// UnitFloorSatoshi is the smallest unit the platform will pay; ladder
	// rungs below it are skipped. The system parameter min_payout_unit.
	UnitFloorSatoshi int64
	// MinUnits is the fewest units a pool is cut into, however congested the
	// network.
	MinUnits int64
	// OutputCap bounds the number of outputs so the payout transaction stays
	// within what the network relays. Outputs never exceed the participants,
	// so the cap only bites when there are more participants than it, and
	// then it bounds the unit count instead.
	OutputCap int64
	// FeeBudgetPercent is the share of the net pool the outputs' own fee may
	// take before the unit is enlarged.
	FeeBudgetPercent int64
	// MaxFeePercent is the miner-fee cap as a share of the pool while the
	// payout is on time.
	MaxFeePercent int64
	// WaivedMaxFeePercent is the cap once the waiting period has passed.
	WaivedMaxFeePercent int64
	// Transaction sizing, in vbytes.
	TxOverheadVBytes int64
	InputVBytes      int64
	OutputVBytes     int64
	OpReturnVBytes   int64
	// SeedBlockMedianFeeRateSatVb is the seed block's median fee rate in whole
	// sat/vB (Bitcoin Core's getblockstats feerate_percentiles, 50th), the
	// one fee rate that is on the chain for anyone to read.
	SeedBlockMedianFeeRateSatVb int64
	// Balances are the participants and their scores.
	Balances []BalanceEntry
}

// UnitsPlatformReported are the platform's own statements about this payout.
// They are published for audit, not reproduced by the verifier.
type UnitsPlatformReported struct {
	// FeeRateMilliSatPerVb is the rate the transaction pays, in thousandths
	// of a sat/vB. 1191 is 1.191 sat/vB.
	FeeRateMilliSatPerVb int64
	// FeeRateSource says where that rate came from: node_estimate or floor.
	FeeRateSource string
	// FeeCapWaived is set once the payout has waited out the deferral window
	// and the higher cap applies.
	FeeCapWaived bool
	// LadderRateSource says where SeedBlockMedianFeeRateSatVb came from: the
	// seed block, or the node's estimate when the block's figure could not be
	// fetched for long enough.
	LadderRateSource string
	// OpReturnVariant says which event summary the transaction carries.
	OpReturnVariant string
}

// UnitsContext is the input to ComputeUnits.
type UnitsContext struct {
	Public   UnitsPublicInputs
	Reported UnitsPlatformReported
}

// UnitsRow is one participant's allocation. Every participant has a row,
// including those paid nothing, so the report lists everyone the pool was
// divided among.
type UnitsRow struct {
	Address string
	Score   int64
	// EntitlementSatoshi is T * score / W rounded down: what the address is
	// owed in expectation.
	EntitlementSatoshi int64
	// UnitsGuaranteed is the whole units of the entitlement, paid outright.
	UnitsGuaranteed int64
	// UnitsExtra is 1 when the sampling landed on this address, else 0.
	UnitsExtra int64
	// FinalRewardSatoshi is what the address is paid, residual included.
	FinalRewardSatoshi int64
}

// UnitsResult is the outcome of ComputeUnits.
type UnitsResult struct {
	PlatformFeeSatoshi   int64
	MinerFeeSatoshi      int64
	DistributableSatoshi int64

	// UnitStepSatoshi is the ladder rung chosen; UnitSatoshi is the value of
	// one unit after fees rounded down (T / K), never below the rung while
	// the fee is within cap. Payouts are T * units / K rounded down per
	// address, so an address holding one unit receives at least UnitSatoshi.
	UnitStepSatoshi int64
	UnitSatoshi     int64
	// UnitCount is K; ExtraUnits is the number settled by sampling.
	UnitCount  int64
	ExtraUnits int64
	// MaxOutputs is the most outputs the transaction could have needed, which
	// is what the fee was reserved for.
	MaxOutputs int64
	// TotalScore is W and DrawOffset the sampling offset, both as decimal
	// strings; DrawOffset is empty when nothing was sampled.
	TotalScore string
	DrawOffset string
	// ResidualSatoshi is what the per-address rounding left over - under one
	// satoshi per address paid - and ResidualAddress who received it.
	ResidualSatoshi int64
	ResidualAddress string
	// Rows are all participants in address order.
	Rows []UnitsRow
}

// PaidRows returns the rows that receive anything, in address order.
func (r *UnitsResult) PaidRows() []UnitsRow {
	paid := make([]UnitsRow, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.FinalRewardSatoshi > 0 {
			paid = append(paid, row)
		}
	}
	return paid
}

// UnitsFeeError says the miner fee is over the cap that applies, or has
// swallowed the pool. It is a reason to come back later, not a failure.
type UnitsFeeError struct {
	FeeSatoshi   int64
	PoolSatoshi  int64
	LimitPercent int64
	Waived       bool
}

func (e *UnitsFeeError) Error() string {
	if e.PoolSatoshi <= 0 {
		return fmt.Sprintf("miner fee %d sats with no pool to pay it from", e.FeeSatoshi)
	}
	return fmt.Sprintf("miner fee %d sats is %d.%02d%% of the %d sat pool, over the %d%% cap (waived=%t)",
		e.FeeSatoshi, e.FeeSatoshi*100/e.PoolSatoshi, (e.FeeSatoshi*10000/e.PoolSatoshi)%100,
		e.PoolSatoshi, e.LimitPercent, e.Waived)
}

// UnitsDustError says the fee has pushed the unit below what a node will
// relay. Paying would strand the payout, so it must not be paid, waived or
// not.
type UnitsDustError struct {
	UnitSatoshi          int64
	UnitCount            int64
	DistributableSatoshi int64
}

func (e *UnitsDustError) Error() string {
	return fmt.Sprintf("unit of %d sats (%d sats over %d units) is below the %d sat dust limit",
		e.UnitSatoshi, e.DistributableSatoshi, e.UnitCount, UnitsDustLimitSatoshi)
}

// Sanity bounds on two public inputs, far above anything the platform
// would set but well inside what the integer arithmetic tolerates: the
// ladder doubles from the floor, and a cap is a transaction output count.
const (
	unitsMaxFloorSatoshi int64 = 1_000_000_000_000 // 10,000 BTC
	unitsMaxOutputCap    int64 = 1_000_000
)

// ErrUnitsNoParticipants is returned when no address has a positive score.
var ErrUnitsNoParticipants = errors.New("no participants")

// ComputeUnits allocates the pool. It is a pure function of its inputs.
func ComputeUnits(ctx UnitsContext) (*UnitsResult, error) {
	p := ctx.Public
	rep := ctx.Reported
	if err := validateUnitsInputs(p, rep); err != nil {
		return nil, err
	}

	participants := unitsEligible(p.Balances)
	if len(participants) == 0 {
		return nil, ErrUnitsNoParticipants
	}
	n := int64(len(participants))

	totalScore := new(big.Int)
	for _, e := range participants {
		totalScore.Add(totalScore, big.NewInt(e.Score))
	}

	// 1. Fees that do not depend on the allocation.
	platformFee := p.PoolSatoshi * p.PlatformFeeBps / 10000
	platformOutputs := int64(0)
	if platformFee > 0 {
		platformOutputs = 1
	}
	netPool := p.PoolSatoshi - platformFee // T0
	if netPool <= 0 {
		return nil, &UnitsFeeError{FeeSatoshi: 0, PoolSatoshi: p.PoolSatoshi, LimitPercent: p.MaxFeePercent}
	}
	capFee := p.PoolSatoshi * p.MaxFeePercent / 100

	// 2. The unit rung, from the seed block's fee rate.
	step := chooseUnitStep(netPool, n, p)

	// 3. The unit count, from public inputs only. It is sized against the
	// worst fee the cap allows, so that as long as the fee stays within cap
	// the unit is worth at least the rung. The output cap bounds it only when
	// there are more participants than the cap: with fewer, outputs are
	// bounded by the participants whatever the unit count.
	unitCount := (netPool - capFee) / step
	if unitCount < 0 {
		unitCount = 0
	}
	if n > p.OutputCap && unitCount > p.OutputCap {
		unitCount = p.OutputCap
	}
	if unitCount < p.MinUnits {
		unitCount = p.MinUnits
	}

	// Guaranteed units and remainders, all big.Int: K * score overflows
	// int64 at ordinary BTC-Time magnitudes.
	bigK := big.NewInt(unitCount)
	guaranteed := make([]int64, len(participants))
	remainders := make([]*big.Int, len(participants))
	var guaranteedTotal int64
	var guaranteedAddresses int64
	for i, e := range participants {
		product := new(big.Int).Mul(bigK, big.NewInt(e.Score))
		q, rem := new(big.Int).QuoRem(product, totalScore, new(big.Int))
		guaranteed[i] = q.Int64()
		remainders[i] = rem
		guaranteedTotal += guaranteed[i]
		if guaranteed[i] > 0 {
			guaranteedAddresses++
		}
	}
	extraUnits := unitCount - guaranteedTotal
	maxOutputs := guaranteedAddresses + extraUnits
	if maxOutputs > n {
		maxOutputs = n
	}

	// 4. The miner fee, reserved for the most outputs the draw can produce.
	// Computed once, before the draw, so the draw cannot move the pool it is
	// dividing.
	minerFee := unitsMinerFee(maxOutputs+platformOutputs, p, rep)
	if !rep.FeeCapWaived && minerFee > capFee {
		return nil, &UnitsFeeError{FeeSatoshi: minerFee, PoolSatoshi: p.PoolSatoshi, LimitPercent: p.MaxFeePercent}
	}
	if rep.FeeCapWaived && minerFee > p.PoolSatoshi*p.WaivedMaxFeePercent/100 {
		return nil, &UnitsFeeError{FeeSatoshi: minerFee, PoolSatoshi: p.PoolSatoshi, LimitPercent: p.WaivedMaxFeePercent, Waived: true}
	}
	distributable := netPool - minerFee
	if distributable <= 0 {
		return nil, &UnitsFeeError{FeeSatoshi: minerFee, PoolSatoshi: p.PoolSatoshi, LimitPercent: p.MaxFeePercent, Waived: rep.FeeCapWaived}
	}

	// 5. The unit's value. The smallest payout is one unit rounded down, and
	// that is what must clear the dust limit.
	unit := distributable / unitCount
	if unit < UnitsDustLimitSatoshi {
		return nil, &UnitsDustError{UnitSatoshi: unit, UnitCount: unitCount, DistributableSatoshi: distributable}
	}

	// 6. Systematic sampling of the extra units. The remainders are laid end
	// to end in address order; their total is exactly extraUnits * W. One
	// offset in [0, W) from the seed, then a point every W: each point falls
	// in exactly one interval, every interval is shorter than W so holds at
	// most one point, and there are exactly extraUnits points on the line.
	extra := make([]int64, len(participants))
	drawOffset := ""
	if extraUnits > 0 {
		offset := newDetRNG(p.Seed).randBig(totalScore)
		drawOffset = offset.String()
		point := new(big.Int).Set(offset)
		lineStart := new(big.Int)
		lineEnd := new(big.Int)
		for i := range participants {
			lineEnd.Add(lineStart, remainders[i])
			for point.Cmp(lineEnd) < 0 {
				if point.Cmp(lineStart) >= 0 {
					extra[i]++
				}
				point.Add(point, totalScore)
			}
			lineStart.Set(lineEnd)
		}
	}

	// 7. Payouts: T * units / K rounded down once per address, so the pool
	// is short by under one satoshi per address paid. That residual goes to
	// the highest score among those paid - never to an address that would
	// otherwise receive nothing, since a residual-only output would be dust.
	rows := make([]UnitsRow, len(participants))
	residualIndex := -1
	bigT := big.NewInt(distributable)
	var paidTotal int64
	for i, e := range participants {
		entitlement := new(big.Int).Mul(bigT, big.NewInt(e.Score))
		entitlement.Quo(entitlement, totalScore)
		units := guaranteed[i] + extra[i]
		paid := new(big.Int).Mul(bigT, big.NewInt(units))
		paid.Quo(paid, bigK)
		rows[i] = UnitsRow{
			Address:            e.Address,
			Score:              e.Score,
			EntitlementSatoshi: entitlement.Int64(),
			UnitsGuaranteed:    guaranteed[i],
			UnitsExtra:         extra[i],
			FinalRewardSatoshi: paid.Int64(),
		}
		paidTotal += rows[i].FinalRewardSatoshi
		if units > 0 && (residualIndex < 0 || e.Score > participants[residualIndex].Score) {
			residualIndex = i
		}
	}
	residual := distributable - paidTotal
	residualAddress := ""
	if residual > 0 && residualIndex >= 0 {
		rows[residualIndex].FinalRewardSatoshi += residual
		residualAddress = rows[residualIndex].Address
	}

	return &UnitsResult{
		PlatformFeeSatoshi:   platformFee,
		MinerFeeSatoshi:      minerFee,
		DistributableSatoshi: distributable,
		UnitStepSatoshi:      step,
		UnitSatoshi:          unit,
		UnitCount:            unitCount,
		ExtraUnits:           extraUnits,
		MaxOutputs:           maxOutputs,
		TotalScore:           totalScore.String(),
		DrawOffset:           drawOffset,
		ResidualSatoshi:      residual,
		ResidualAddress:      residualAddress,
		Rows:                 rows,
	}, nil
}

// chooseUnitStep walks the ladder until the outputs' own fee, priced at the
// seed block's median rate, fits the fee budget - or until the pool is worth
// no more than one rung, where the unit count is pinned at the minimum
// anyway.
func chooseUnitStep(netPool int64, participants int64, p UnitsPublicInputs) int64 {
	for i := 0; ; i++ {
		step := UnitLadderStep(i)
		if step < p.UnitFloorSatoshi {
			continue
		}
		outputs := netPool / step
		if outputs > participants {
			outputs = participants
		}
		if outputs > p.OutputCap {
			outputs = p.OutputCap
		}
		feeOfOutputs := outputs * p.OutputVBytes * p.SeedBlockMedianFeeRateSatVb
		if feeOfOutputs*100 <= netPool*p.FeeBudgetPercent || netPool/step <= 1 {
			return step
		}
	}
}

// unitsMinerFee prices a transaction with the given number of outputs at the
// reported rate, rounding up: a fee a fraction short of the rate claimed is
// not worth a node disagreeing.
func unitsMinerFee(outputs int64, p UnitsPublicInputs, rep UnitsPlatformReported) int64 {
	vbytes := p.TxOverheadVBytes + p.InputVBytes + p.OutputVBytes*outputs + p.OpReturnVBytes
	fee := vbytes * rep.FeeRateMilliSatPerVb
	return (fee + 999) / 1000
}

// unitsEligible de-duplicates by address, drops non-positive scores and sorts
// by address so that the sampling line is the same wherever it is built.
func unitsEligible(balances []BalanceEntry) []BalanceEntry {
	agg := make(map[string]int64, len(balances))
	for _, b := range balances {
		if b.Address == "" || b.Score <= 0 {
			continue
		}
		agg[b.Address] += b.Score
	}
	out := make([]BalanceEntry, 0, len(agg))
	for address, score := range agg {
		out = append(out, BalanceEntry{Address: address, Score: score})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func validateUnitsInputs(p UnitsPublicInputs, rep UnitsPlatformReported) error {
	switch {
	case p.Seed == "":
		return errors.New("seed must not be empty")
	case p.PoolSatoshi <= 0:
		return errors.New("pool must be > 0")
	case p.PlatformFeeBps < 0 || p.PlatformFeeBps >= 10000:
		return errors.New("platform fee must be within [0, 10000) bps")
	case p.UnitFloorSatoshi < UnitsDustLimitSatoshi:
		return fmt.Errorf("unit floor must be at least %d sats", UnitsDustLimitSatoshi)
	case p.UnitFloorSatoshi > unitsMaxFloorSatoshi:
		return fmt.Errorf("unit floor must be at most %d sats", unitsMaxFloorSatoshi)
	case p.MinUnits < 1:
		return errors.New("min units must be >= 1")
	case p.OutputCap < p.MinUnits:
		return errors.New("output cap must be >= min units")
	case p.OutputCap > unitsMaxOutputCap:
		return fmt.Errorf("output cap must be at most %d", unitsMaxOutputCap)
	case p.FeeBudgetPercent < 0:
		return errors.New("fee budget must be >= 0")
	case p.MaxFeePercent <= 0 || p.MaxFeePercent > 100:
		return errors.New("max fee percent must be within (0, 100]")
	case p.WaivedMaxFeePercent < p.MaxFeePercent || p.WaivedMaxFeePercent > 100:
		return errors.New("waived max fee percent must be within [max fee percent, 100]")
	case p.TxOverheadVBytes < 0 || p.InputVBytes < 0 || p.OutputVBytes <= 0 || p.OpReturnVBytes < 0:
		return errors.New("transaction sizes must be non-negative and output size positive")
	case p.SeedBlockMedianFeeRateSatVb < 0:
		return errors.New("seed block fee rate must be >= 0")
	case rep.FeeRateMilliSatPerVb <= 0:
		return errors.New("fee rate must be > 0")
	}
	return nil
}
