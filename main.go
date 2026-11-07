package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/koinvote/event-verifier/service"
)

// maxSupportedSchemaVersion is the newest report format this build knows how
// to check.
//
// A newer file is refused rather than read with the rules of an older one. The
// version exists because the rules changed - version 2 stopped deriving the fee
// rate and started auditing declared policy instead, version 3 split the
// settlement block into two - so reading a newer file with an older version's
// rules would produce a "passed" that means nothing. A tool that cannot tell
// the difference between "checked and correct" and "did not understand the
// file" is worse than no tool.
//
// Version 4 adds op_return_vbytes, the size of the event summary the payout
// transaction carries. It is part of the miner fee, so an older build would
// recompute a smaller fee and report every new payout as wrong.
//
// Version 5 replaces the draw. A report that names distribution_algorithm
// units_v2 pays every address its share of the score in whole units and
// settles the fractions by one sampled offset (service/lottery_units.go);
// the header carries the unit ladder inputs, the seed block's median fee
// rate, the waiver flag and the block times a waived payout is held to. An
// older build would replay the lottery over a report that never ran one.
const maxSupportedSchemaVersion = 5

type headerRow struct {
	// schemaVersion is absent in every report written before the fee model
	// changed; those are version 1 and are read exactly as they always were.
	schemaVersion    int
	eventID          string
	payoutTxID       string
	feeRateDecimal   float64
	feeRateSource    string
	feeTargetBlocks  int
	minFeeRateSatVb  float64
	maxFeePercentage float64
	seed             string

	// settlementBlockHeight is the version 1 and 2 shape: one block that both
	// ended the scoring window and supplied the seed. Absent in reports
	// predating BTC-Time scoring, and absent from version 3 onwards.
	settlementBlockHeight int64

	// From version 3 the two are separate blocks. scoreBlockHeight ends the
	// scoring window; seedBlockHeight is where the seed above came from, six
	// blocks later, so that whoever mined it could not see the balances being
	// weighted.
	scoreBlockHeight      int64
	scoreBlockHash        string
	seedBlockHeight       int64
	poolSatoshi           int64
	winnerCount           int
	platformFeePercentage float64
	dustThresholdSatoshi  int64
	payoutFeeMultiplier   float64
	feeRateSatVb          int64
	txOverheadVBytes      int
	inputP2WSHBytes       int
	outputDefaultVBytes   int
	// opReturnVBytes is the space the event summary takes in the payout
	// transaction, from version 4. Zero before that.
	opReturnVBytes int

	// Version 5: the units_v2 allocation. distributionAlgorithm names the
	// code path; the rest are its inputs and the figures it claims, split as
	// service/lottery_units.go splits them - public inputs this tool
	// recomputes from, and the platform's own statements it holds to policy.
	distributionAlgorithm       string
	feeRateMilliSatVb           int64
	waivedMaxFeePercentage      float64
	feeCapWaived                bool
	opReturnVariant             string
	unitFloorSatoshi            int64
	minUnits                    int64
	outputCap                   int64
	feeBudgetPercentage         float64
	seedBlockMedianFeeRateSatVb int64
	ladderRateSource            string
	unitStepSatoshi             int64
	unitSatoshi                 int64
	unitCount                   int64
	extraUnits                  int64
	maxOutputs                  int64
	drawOffset                  string
	residualSatoshi             int64
	residualAddress             string
	// seedBlockMTP is the seed block's median time past; payoutBlockHeight
	// and payoutBlockMTP name the block the payout transaction confirmed in,
	// filled in after confirmation. Together they let a waived fee cap be
	// checked against the 72-hour wait it requires, offline.
	seedBlockMTP      int64
	payoutBlockHeight int64
	payoutBlockMTP    int64
}

// columns maps a section's column names to their position, taken from the
// "type,..." row that precedes each section in the report.
//
// Reading by name instead of by fixed offset is deliberate. Version 1.0.0 read
// fixed offsets, so when the report gained a referral_code column every field
// after it shifted by one and the tool reported a perfectly good payout as
// unverifiable. Names let one build read both the old reports and the current
// ones, and survive the next column that gets added.
type columns map[string]int

func newColumns(record []string) columns {
	c := make(columns, len(record))
	for i, name := range record {
		c[strings.ToLower(strings.TrimSpace(name))] = i
	}
	return c
}

// lookup returns the first of the given column names present in the row.
// Several names are accepted per field so that a column which has been renamed
// is still found under its former name.
func (c columns) lookup(record []string, names ...string) (string, bool) {
	for _, name := range names {
		if i, ok := c[name]; ok && i < len(record) {
			return strings.TrimSpace(record[i]), true
		}
	}
	return "", false
}

// require fails loudly when a column the verification actually needs is absent,
// naming it so the reader can tell a stale tool from a malformed report.
func (c columns) require(record []string, section string, names ...string) string {
	value, ok := c.lookup(record, names...)
	if !ok {
		panic(fmt.Errorf("%s row has no %s column (tool may be older than the report)", section, names[0]))
	}
	return value
}

type resultSummary struct {
	platformFeeSatoshi       int64
	estimatedMinerFeeSatoshi int64
	distributableSatoshi     int64
	feeRateSatVb             float64
}

type resultWinner struct {
	address string
	// weight is the lottery weight the report claims for this address. Under
	// BTC-Time scoring it is holding_score_sat_blocks; in reports predating it,
	// balance_satoshi. The two are different units but play the same role, and
	// the arithmetic below never mixes them: both sides of every comparison
	// come from the same report.
	weight                int64
	originalRewardSatoshi int64
	finalRewardSatoshi    int64
	isDust                bool
	// Version 5 columns. Under units_v2 every participant has a row, and
	// these say what it was owed and how many units it received.
	entitlementSatoshi int64
	unitsGuaranteed    int64
	unitsExtra         int64
}

type parsedCSV struct {
	header    headerRow
	balances  []service.BalanceEntry
	summary   resultSummary
	winners   []resultWinner
	hasHeader bool
	hasResult bool

	// sha256 of the exact bytes that were verified.
	//
	// "Verification passed" only says the arithmetic in this file is
	// self-consistent. It cannot say the file is the one that was published,
	// because the tool has no way to know what was published - so it prints the
	// digest and leaves that comparison to the reader, who does.
	sha256 string
	// unknownRowTypes are row kinds this build does not recognise, kept so the
	// schema-version check can produce its better message first.
	unknownRowTypes []string
}

// maxReportBytes bounds what will be read.
//
// Without it `--report /dev/zero` never returns and the user sees a tool that
// hung with no output. A payout report is one row per participant; the largest
// event to date is under 40 KB, and this allows four thousand times that.
const maxReportBytes = 64 << 20

// toolVersion tracks the newest report format this build reads, which is the
// only question --version is asked to settle: someone holding a report and a
// binary needs to know whether the one can check the other. It moves with
// maxSupportedSchemaVersion above, so 5.0.0 reads version 5 and everything
// before it.
const toolVersion = "5.0.0"

func main() {
	reportPath := flag.String("report", "", "path to payout verification CSV")
	verbose := flag.Bool("verbose", false, "show detailed verification output")
	showVersion := flag.Bool("version", false, "show tool version")
	flag.Parse()

	if *showVersion {
		fmt.Println(toolVersion)
		return
	}

	defer func() {
		if r := recover(); r != nil {
			printVerificationFailed("report file is incorrect or incomplete")
			if *verbose {
				fmt.Printf("Report parse panic: %v\n", r)
			}
			os.Exit(1)
		}
	}()

	if strings.TrimSpace(*reportPath) == "" {
		printVerificationFailed("input parameters are incorrect")
		if *verbose {
			fmt.Println("Missing required flag: --report")
		}
		os.Exit(2)
	}

	parsed, err := loadReport(*reportPath)
	if err != nil {
		printVerificationFailed("report file is incorrect or incomplete")
		if *verbose {
			fmt.Printf("Failed to parse report: %s\n", err)
		}
		os.Exit(1)
	}

	issues, err := verifyReport(parsed)
	if err != nil {
		printVerificationFailed("input parameters are incorrect")
		if *verbose {
			fmt.Printf("Failed to compute the payout: %s\n", err)
		}
		os.Exit(1)
	}

	if len(issues) == 0 {
		fmt.Printf("%s The lottery result matches the report.\n",
			colourise("Verification passed.", ansiBoldGreen))
		printRemainingChecks(parsed)
		return
	}

	printVerificationFailed("tool version mismatch")
	if *verbose {
		fmt.Println("Detailed mismatches:")
		for _, issue := range issues {
			fmt.Printf("- %s\n", issue)
		}
	}
	os.Exit(1)
}

func loadReport(path string) (parsed *parsedCSV, err error) {
	// The helpers below signal a malformed field by panicking, which keeps the
	// parsing code readable but would otherwise surface as a Go stack trace in
	// front of someone checking their own payout. A truncated download or the
	// wrong file picked out of a folder is an ordinary mistake, and it has to
	// read as "this file could not be understood", not as the tool falling over.
	defer func() {
		if r := recover(); r != nil {
			parsed = nil
			if e, ok := r.(error); ok {
				err = fmt.Errorf("malformed report: %w", e)
				return
			}
			err = fmt.Errorf("malformed report: %v", r)
		}
	}()

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open report: %w", err)
	}
	defer file.Close()

	// Read one byte past the limit so hitting it is distinguishable from a file
	// that happens to be exactly that size. Reading it all in also gives the
	// digest below the exact bytes that were parsed, rather than a second read
	// of a file that might have changed in between.
	raw, err := io.ReadAll(io.LimitReader(file, maxReportBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read report: %w", err)
	}
	if len(raw) > maxReportBytes {
		return nil, fmt.Errorf("report is larger than %d MB; a payout report is a few tens of kilobytes, so this is not one",
			maxReportBytes>>20)
	}

	digest := sha256.Sum256(raw)

	// Opening a report in Excel and saving it adds a UTF-8 byte order mark. The
	// content is unchanged, so refusing it would report a tampered file when
	// nothing was tampered with.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})

	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1

	parsed = &parsedCSV{sha256: hex.EncodeToString(digest[:])}
	var cols columns
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read csv: %w", err)
		}
		if len(record) == 0 {
			continue
		}
		// Each section is preceded by its own column-name row. Remember it and
		// read the rows that follow by name.
		if strings.ToLower(strings.TrimSpace(record[0])) == "type" {
			cols = newColumns(record)
			continue
		}
		if cols == nil {
			return nil, fmt.Errorf("data row appears before any column-name row")
		}

		switch strings.ToLower(strings.TrimSpace(record[0])) {
		case "header":
			parsed.header.seed = cols.require(record, "header", "seed")
			// Version 1 and 2 carry one settlement height; version 3 carries
			// two blocks. Both shapes are read here, by name, so one build
			// verifies every report this tool has ever been given.
			if raw, ok := cols.lookup(record, "settlement_block_height"); ok && raw != "" {
				parsed.header.settlementBlockHeight = mustInt64(raw, "settlement_block_height")
			}
			if raw, ok := cols.lookup(record, "score_block_height"); ok && raw != "" {
				parsed.header.scoreBlockHeight = mustInt64(raw, "score_block_height")
			}
			parsed.header.scoreBlockHash, _ = cols.lookup(record, "score_block_hash")
			if raw, ok := cols.lookup(record, "seed_block_height"); ok && raw != "" {
				parsed.header.seedBlockHeight = mustInt64(raw, "seed_block_height")
			}
			parsed.header.poolSatoshi = mustInt64(cols.require(record, "header", "pool_satoshi"), "pool_satoshi")
			parsed.header.winnerCount = mustInt(cols.require(record, "header", "winner_count"), "winner_count")
			parsed.header.platformFeePercentage = mustFloat(cols.require(record, "header", "platform_fee_percentage"), "platform_fee_percentage")
			parsed.header.dustThresholdSatoshi = mustInt64(cols.require(record, "header", "dust_threshold_satoshi"), "dust_threshold_satoshi")
			// From schema 2 the header states the rate outright and drops the
			// multiplier it used to be derived from. Both shapes are read here
			// so one build verifies old and new reports alike.
			if raw, ok := cols.lookup(record, "schema_version"); ok && raw != "" {
				parsed.header.schemaVersion = mustInt(raw, "schema_version")
			} else {
				parsed.header.schemaVersion = 1
			}
			parsed.header.eventID, _ = cols.lookup(record, "event_id")
			parsed.header.payoutTxID, _ = cols.lookup(record, "payout_txid")

			if parsed.header.schemaVersion >= 2 {
				parsed.header.feeRateDecimal = mustFloat(cols.require(record, "header", "fee_rate_sat_vb"), "fee_rate_sat_vb")
				parsed.header.feeRateSource, _ = cols.lookup(record, "fee_rate_source")
				if raw, ok := cols.lookup(record, "fee_target_blocks"); ok && raw != "" {
					parsed.header.feeTargetBlocks = mustInt(raw, "fee_target_blocks")
				}
				if raw, ok := cols.lookup(record, "min_fee_rate_sat_vb"); ok && raw != "" {
					parsed.header.minFeeRateSatVb = mustFloat(raw, "min_fee_rate_sat_vb")
				}
				if raw, ok := cols.lookup(record, "max_fee_percentage"); ok && raw != "" {
					parsed.header.maxFeePercentage = mustFloat(raw, "max_fee_percentage")
				}
			} else {
				parsed.header.payoutFeeMultiplier = mustFloat(cols.require(record, "header", "payout_fee_multiplier"), "payout_fee_multiplier")
				parsed.header.feeRateSatVb = mustInt64(cols.require(record, "header", "fee_rate_sat_vb"), "fee_rate_sat_vb")
			}
			parsed.header.txOverheadVBytes = mustInt(cols.require(record, "header", "tx_overhead_vbytes"), "tx_overhead_vbytes")
			parsed.header.inputP2WSHBytes = mustInt(cols.require(record, "header", "input_p2wsh_bytes"), "input_p2wsh_bytes")
			parsed.header.outputDefaultVBytes = mustInt(cols.require(record, "header", "output_default_vbytes"), "output_default_vbytes")
			// Required, not optional, from version 4: a version 4 report
			// without it would otherwise verify against a fee that leaves the
			// summary out.
			if parsed.header.schemaVersion >= 4 {
				parsed.header.opReturnVBytes = mustInt(cols.require(record, "header", "op_return_vbytes"), "op_return_vbytes")
			}
			// Version 5 names its allocation and, for units_v2, every input the
			// allocation is recomputed from. All required: a version 5 report
			// missing one would otherwise be replayed with a default the
			// backend never used.
			if parsed.header.schemaVersion >= 5 {
				h := &parsed.header
				h.distributionAlgorithm = cols.require(record, "header", "distribution_algorithm")
				if h.distributionAlgorithm == service.UnitsAlgorithmID {
					h.feeRateMilliSatVb = mustInt64(cols.require(record, "header", "fee_rate_milli_sat_vb"), "fee_rate_milli_sat_vb")
					h.waivedMaxFeePercentage = mustFloat(cols.require(record, "header", "waived_max_fee_percentage"), "waived_max_fee_percentage")
					h.feeCapWaived = mustBool(cols.require(record, "header", "fee_cap_waived"), "fee_cap_waived")
					h.opReturnVariant, _ = cols.lookup(record, "op_return_variant")
					h.unitFloorSatoshi = mustInt64(cols.require(record, "header", "unit_floor_satoshi"), "unit_floor_satoshi")
					h.minUnits = mustInt64(cols.require(record, "header", "min_units"), "min_units")
					h.outputCap = mustInt64(cols.require(record, "header", "output_cap"), "output_cap")
					h.feeBudgetPercentage = mustFloat(cols.require(record, "header", "fee_budget_percentage"), "fee_budget_percentage")
					h.seedBlockMedianFeeRateSatVb = mustInt64(cols.require(record, "header", "seed_block_median_fee_rate_sat_vb"), "seed_block_median_fee_rate_sat_vb")
					h.ladderRateSource = cols.require(record, "header", "ladder_rate_source")
					h.unitStepSatoshi = mustInt64(cols.require(record, "header", "unit_step_satoshi"), "unit_step_satoshi")
					h.unitSatoshi = mustInt64(cols.require(record, "header", "unit_satoshi"), "unit_satoshi")
					h.unitCount = mustInt64(cols.require(record, "header", "unit_count"), "unit_count")
					h.extraUnits = mustInt64(cols.require(record, "header", "extra_units"), "extra_units")
					h.maxOutputs = mustInt64(cols.require(record, "header", "max_outputs"), "max_outputs")
					h.drawOffset = cols.require(record, "header", "draw_offset")
					h.residualSatoshi = mustInt64(cols.require(record, "header", "residual_satoshi"), "residual_satoshi")
					h.residualAddress = cols.require(record, "header", "residual_address")
					h.seedBlockMTP = mustInt64(cols.require(record, "header", "seed_block_mtp"), "seed_block_mtp")
					if raw, ok := cols.lookup(record, "payout_block_height"); ok && raw != "" {
						h.payoutBlockHeight = mustInt64(raw, "payout_block_height")
					}
					if raw, ok := cols.lookup(record, "payout_block_mtp"); ok && raw != "" {
						h.payoutBlockMTP = mustInt64(raw, "payout_block_mtp")
					}
				}
			}
			parsed.hasHeader = true
		case "balance":
			parsed.balances = append(parsed.balances, service.BalanceEntry{
				Address: cols.require(record, "balance", "address"),
				Score:   mustInt64(cols.require(record, "balance", "holding_score_sat_blocks", "balance_satoshi"), "holding_score_sat_blocks"),
			})
		case "result_summary":
			parsed.summary.platformFeeSatoshi = mustInt64(cols.require(record, "result_summary", "platform_fee_satoshi"), "platform_fee_satoshi")
			parsed.summary.estimatedMinerFeeSatoshi = mustInt64(cols.require(record, "result_summary", "estimated_miner_fee_satoshi"), "estimated_miner_fee_satoshi")
			parsed.summary.distributableSatoshi = mustInt64(cols.require(record, "result_summary", "distributable_satoshi"), "distributable_satoshi")
			parsed.summary.feeRateSatVb = mustFloat(cols.require(record, "result_summary", "fee_rate_sat_vb"), "fee_rate_sat_vb")
			parsed.hasResult = true
		case "result_winner":
			winner := resultWinner{
				address:               cols.require(record, "result_winner", "address"),
				weight:                mustInt64(cols.require(record, "result_winner", "holding_score_sat_blocks", "balance_satoshi"), "holding_score_sat_blocks"),
				originalRewardSatoshi: mustInt64(cols.require(record, "result_winner", "original_reward_satoshi"), "original_reward_satoshi"),
				finalRewardSatoshi:    mustInt64(cols.require(record, "result_winner", "final_reward_satoshi"), "final_reward_satoshi"),
				isDust:                mustBool(cols.require(record, "result_winner", "is_dust"), "is_dust"),
			}
			if parsed.header.distributionAlgorithm == service.UnitsAlgorithmID {
				winner.entitlementSatoshi = mustInt64(cols.require(record, "result_winner", "entitlement_satoshi"), "entitlement_satoshi")
				winner.unitsGuaranteed = mustInt64(cols.require(record, "result_winner", "units_guaranteed"), "units_guaranteed")
				winner.unitsExtra = mustInt64(cols.require(record, "result_winner", "units_extra"), "units_extra")
			}
			parsed.winners = append(parsed.winners, winner)

		default:
			// Anything appended to a valid report used to land here and be
			// dropped, so a file with extra bytes on the end still verified.
			// A tool that cannot account for what it read has no business
			// saying the file checks out.
			parsed.unknownRowTypes = appendUnique(parsed.unknownRowTypes, strings.TrimSpace(record[0]))
		}
	}

	if !parsed.hasHeader {
		return nil, fmt.Errorf("missing header row")
	}
	if parsed.header.schemaVersion > maxSupportedSchemaVersion {
		return nil, fmt.Errorf(
			"report uses schema version %d but this build only understands up to %d; "+
				"download the current verifier from https://github.com/koinvote/event-verifier",
			parsed.header.schemaVersion, maxSupportedSchemaVersion)
	}
	// After the schema check on purpose: a report from a newer build is also
	// full of rows this one does not know, and "download the current verifier"
	// is the useful thing to say about it.
	if len(parsed.unknownRowTypes) > 0 {
		// Quoted because the value came from the file and may be arbitrary
		// bytes; the point of this message is that something unexpected is in
		// there, and printing it raw would put it straight into a terminal.
		quoted := make([]string, 0, len(parsed.unknownRowTypes))
		for _, t := range parsed.unknownRowTypes {
			quoted = append(quoted, fmt.Sprintf("%q", t))
		}
		return nil, fmt.Errorf("report contains %d row kind(s) this build does not understand (%s); "+
			"it may have been edited or truncated",
			len(parsed.unknownRowTypes), strings.Join(quoted, ", "))
	}
	if !parsed.hasResult {
		return nil, fmt.Errorf("missing result_summary row")
	}
	switch parsed.header.distributionAlgorithm {
	case "", "lottery_v1", service.UnitsAlgorithmID:
	default:
		return nil, fmt.Errorf("report names distribution_algorithm %q, which this build does not know; "+
			"download the current verifier from https://github.com/koinvote/event-verifier",
			parsed.header.distributionAlgorithm)
	}
	if len(parsed.balances) == 0 {
		return nil, fmt.Errorf("missing balance rows")
	}

	return parsed, nil
}

// checkFeePolicy audits what a version 2 report can still be held to.
//
// Version 1 priced the fee from a constant and a multiplier, both stated in the
// report, so this tool could recompute the rate and prove it. Version 2 takes
// the rate from the node's fee estimator at the moment of planning, and no one
// can reproduce that later - the mempool it described no longer exists.
//
// What can still be proven is that the payout obeyed the rules it published:
// the rate is at or above the floor it declares, it is the same figure in both
// sections, and the fee it charged is no more of the pool than it said it would
// take. That is a weaker claim than version 1's arithmetic check, and it is the
// honest one - the alternative was to keep pretending a hardcoded constant was
// a market rate.
// scoreBlockOffset is the fixed distance between the block that supplied the
// seed and the block scoring stopped at.
//
// Two blocks rather than one, because they answer different questions: the
// scoring block decides whose balance counts, the seed block decides who wins.
// Fixing the weights six blocks before the seed exists means the miner of the
// seed block cannot see what their hash would pay out - the weights were
// already settled when they started mining.
//
// The backend's own constant, restated here. This tool already restates the
// draw itself; a rule that decides which block may be used is no different, and
// leaving it out meant the two heights were printed and never questioned.
const scoreBlockOffset = 6

// checkSettlementBlocks verifies the one thing about the settlement blocks that
// can be checked without touching a chain: their distance from each other.
//
// Whether either block is real is for the reader and a block explorer. Whether
// they are the right distance apart is arithmetic, and it was going unchecked -
// a report could name any pair of heights and this tool would print them under
// "verification passed" without comment.
func checkSettlementBlocks(parsed *parsedCSV) []string {
	h := parsed.header

	// Version 1 and 2 settle on one block that does both jobs, so there is no
	// distance to check.
	if h.seedBlockHeight == 0 || h.scoreBlockHeight == 0 {
		return nil
	}

	if gap := h.seedBlockHeight - h.scoreBlockHeight; gap != scoreBlockOffset {
		return []string{fmt.Sprintf(
			"settlement blocks are %d apart: score_block_height=%d seed_block_height=%d, expected a gap of %d",
			gap, h.scoreBlockHeight, h.seedBlockHeight, scoreBlockOffset)}
	}
	return nil
}

func checkFeePolicy(parsed *parsedCSV) []string {
	if parsed.header.schemaVersion < 2 {
		return nil
	}

	issues := make([]string, 0)

	if parsed.header.feeRateDecimal != parsed.summary.feeRateSatVb {
		issues = append(issues, fmt.Sprintf(
			"fee rate disagrees between sections: header=%v result_summary=%v",
			parsed.header.feeRateDecimal, parsed.summary.feeRateSatVb))
	}

	if parsed.header.minFeeRateSatVb > 0 && parsed.header.feeRateDecimal < parsed.header.minFeeRateSatVb {
		issues = append(issues, fmt.Sprintf(
			"fee rate %v is below the floor the report declares (%v)",
			parsed.header.feeRateDecimal, parsed.header.minFeeRateSatVb))
	}

	// A rate that was clamped must say so, and one that was not must not
	// claim it was.
	switch parsed.header.feeRateSource {
	case "floor":
		if parsed.header.minFeeRateSatVb > 0 && parsed.header.feeRateDecimal != parsed.header.minFeeRateSatVb {
			issues = append(issues, fmt.Sprintf(
				"fee_rate_source says floor but the rate is %v, not the floor %v",
				parsed.header.feeRateDecimal, parsed.header.minFeeRateSatVb))
		}
	case "node_estimate", "":
	default:
		issues = append(issues, fmt.Sprintf("unknown fee_rate_source %q", parsed.header.feeRateSource))
	}

	// Under units_v2 the cap depends on whether the waiting period has passed,
	// so it is checked in checkUnitsPolicy instead.
	if parsed.header.distributionAlgorithm != service.UnitsAlgorithmID && parsed.header.maxFeePercentage > 0 && parsed.header.poolSatoshi > 0 {
		cap := float64(parsed.header.poolSatoshi) * parsed.header.maxFeePercentage / 100
		if float64(parsed.summary.estimatedMinerFeeSatoshi) > cap {
			issues = append(issues, fmt.Sprintf(
				"miner fee %d sats is %.2f%% of the pool, over the %.2f%% cap the report declares",
				parsed.summary.estimatedMinerFeeSatoshi,
				float64(parsed.summary.estimatedMinerFeeSatoshi)/float64(parsed.header.poolSatoshi)*100,
				parsed.header.maxFeePercentage))
		}
	}

	return issues
}

func compareResults(parsed *parsedCSV, result *service.LotteryResult) []string {
	issues := make([]string, 0)

	if parsed.summary.platformFeeSatoshi != result.PlatformFeeSatoshi {
		issues = append(issues, fmt.Sprintf("platform_fee_satoshi mismatch: report=%d computed=%d", parsed.summary.platformFeeSatoshi, result.PlatformFeeSatoshi))
	}
	if parsed.summary.estimatedMinerFeeSatoshi != result.EstimatedMinerFeeSatoshi {
		issues = append(issues, fmt.Sprintf("estimated_miner_fee_satoshi mismatch: report=%d computed=%d", parsed.summary.estimatedMinerFeeSatoshi, result.EstimatedMinerFeeSatoshi))
	}
	if parsed.summary.distributableSatoshi != result.DistributableSatoshi {
		issues = append(issues, fmt.Sprintf("distributable_satoshi mismatch: report=%d computed=%d", parsed.summary.distributableSatoshi, result.DistributableSatoshi))
	}
	if parsed.summary.feeRateSatVb != result.FeeRateSatVb {
		issues = append(issues, fmt.Sprintf("fee_rate_sat_vb mismatch: report=%v computed=%v", parsed.summary.feeRateSatVb, result.FeeRateSatVb))
	}

	issues = append(issues, checkFeePolicy(parsed)...)
	issues = append(issues, checkSettlementBlocks(parsed)...)

	computed := make(map[string]service.WinnerResult, len(result.Winners))
	for _, winner := range result.Winners {
		computed[winner.Address] = winner
	}

	if len(parsed.winners) != len(computed) {
		issues = append(issues, fmt.Sprintf("winner count mismatch: report=%d computed=%d", len(parsed.winners), len(computed)))
	}

	for _, winner := range parsed.winners {
		computedWinner, ok := computed[winner.address]
		if !ok {
			issues = append(issues, fmt.Sprintf("missing winner in computed result: %s", winner.address))
			continue
		}

		if winner.weight != computedWinner.Score {
			issues = append(issues, fmt.Sprintf("lottery weight mismatch for %s: report=%d computed=%d", winner.address, winner.weight, computedWinner.Score))
		}
		if winner.originalRewardSatoshi != computedWinner.OriginalRewardSatoshi {
			issues = append(issues, fmt.Sprintf("original_reward_satoshi mismatch for %s: report=%d computed=%d", winner.address, winner.originalRewardSatoshi, computedWinner.OriginalRewardSatoshi))
		}
		if winner.finalRewardSatoshi != computedWinner.FinalRewardSatoshi {
			issues = append(issues, fmt.Sprintf("final_reward_satoshi mismatch for %s: report=%d computed=%d", winner.address, winner.finalRewardSatoshi, computedWinner.FinalRewardSatoshi))
		}
		if winner.isDust != computedWinner.IsDust {
			issues = append(issues, fmt.Sprintf("is_dust mismatch for %s: report=%t computed=%t", winner.address, winner.isDust, computedWinner.IsDust))
		}
	}

	if len(parsed.winners) > 0 && len(computed) > 0 {
		computedAddresses := make([]string, 0, len(computed))
		for address := range computed {
			computedAddresses = append(computedAddresses, address)
		}
		sort.Strings(computedAddresses)

		reportAddresses := make([]string, 0, len(parsed.winners))
		for _, winner := range parsed.winners {
			reportAddresses = append(reportAddresses, winner.address)
		}
		sort.Strings(reportAddresses)

		if strings.Join(reportAddresses, ",") != strings.Join(computedAddresses, ",") {
			issues = append(issues, "winner address sets differ")
		}
	}

	return issues
}

func mustInt64(raw string, label string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		panic(newParseError(label, raw))
	}
	return value
}

func mustInt(raw string, label string) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		panic(newParseError(label, raw))
	}
	return value
}

func mustFloat(raw string, label string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		panic(newParseError(label, raw))
	}
	return value
}

func mustBool(raw string, label string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		panic(newParseError(label, raw))
	}
	return value
}

// Bold rather than plain colour: the verdict has to survive a terminal whose
// palette has been remapped, and weight carries when hue does not.
const (
	ansiBoldGreen = "\033[1;32m"
	ansiBoldRed   = "\033[1;31m"
	ansiReset     = "\033[0m"
)

// colourise wraps text in an escape sequence only when stdout is a terminal.
//
// The verdict line is the part of this tool people quote - pasted into a
// message, piped through grep, redirected to a file kept beside the report.
// Escape bytes written into any of those turn "Verification passed." into
// something that reads as corrupted, which is a bad look for a tool whose whole
// job is telling you whether bytes were tampered with.
func colourise(text string, colour string) string {
	info, err := os.Stdout.Stat()
	if err != nil {
		return text
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return text
	}
	return colour + text + ansiReset
}

func printVerificationFailed(reason string) {
	fmt.Println(colourise("Verification failed.", ansiBoldRed))
	fmt.Println("Possible causes:")
	fmt.Println("- Report file is incorrect or incomplete")
	fmt.Println("- Tool version mismatch")
	fmt.Println("- Input parameters are incorrect")
	_ = reason
}

func newParseError(label string, raw string) error {
	return fmt.Errorf("invalid %s: %s", label, raw)
}

// printSettlementBlocks names the blocks a report was settled on, so the two
// heights are something a reader can act on rather than columns parsed and
// discarded.
//
// This tool does not reach the network, so it cannot confirm that the seed
// really is the hash of the block at that height - only that the draw follows
// from the seed the report states. Naming the height is what lets someone go
// and check that last step on a block explorer.
// printRemainingChecks names what is left for the reader to do.
//
// Only things they can act on. The scoring block height used to be printed too
// and there is nothing to do with it: no explorer can say whether a height is
// the right cutoff, and its one checkable property - the distance to the seed
// block - is now checked here rather than recited.
//
// Everything in the file is the report's own claim; that is what a verifier is
// for. What earns a line is being the reader's cheapest way to test one, and
// these two are minutes of work against a claim that would otherwise rest on
// trust: a seed that was chosen rather than taken from a block picks the
// winners outright, and a file that is not the published one makes the rest of
// this output describe some other document.
func printRemainingChecks(parsed *parsedCSV) {
	h := parsed.header

	switch {
	case h.seedBlockHeight > 0:
		fmt.Printf("  seed came from block %d (%s) - check it in a block explorer\n",
			h.seedBlockHeight, h.seed)
	case h.settlementBlockHeight > 0:
		// Version 1 and 2: one block did both jobs.
		fmt.Printf("  seed came from block %d (%s) - check it in a block explorer\n",
			h.settlementBlockHeight, h.seed)
	default:
		fmt.Printf("  seed is %s - the report does not say which block it came from\n", h.seed)
	}

	if h.distributionAlgorithm == service.UnitsAlgorithmID {
		fmt.Printf("  the unit ladder used a seed-block median fee rate of %d sat/vB - check it with:\n"+
			"    bitcoin-cli getblockstats %s '[\"feerate_percentiles\"]'   (the third value)\n",
			h.seedBlockMedianFeeRateSatVb, h.seed)
		if h.ladderRateSource == service.UnitsLadderRateNodeEstimate {
			fmt.Println("  note: the report says the seed block's rate could not be fetched and the node's estimate sized the ladder instead")
		}
		switch {
		case h.feeCapWaived && h.payoutBlockMTP > 0:
			fmt.Printf("  the fee cap was waived after a 72-hour wait: seed block %d (mtp %d) to payout block %d (mtp %d) - check both blocks in a block explorer\n",
				h.seedBlockHeight, h.seedBlockMTP, h.payoutBlockHeight, h.payoutBlockMTP)
		case h.feeCapWaived:
			fmt.Println("  the fee cap was waived, but the payout block is not in this report yet, so the 72-hour wait cannot be checked until it is")
		}
	}

	fmt.Printf("  SHA-256 of this file: %s - compare with the digest beside the download link\n",
		parsed.sha256)
}

// verifyReport recomputes the payout the way the report says it was made and
// returns every disagreement. One dispatcher for main and the tests, so both
// replay exactly the computation the report names.
func verifyReport(parsed *parsedCSV) ([]string, error) {
	if parsed.header.distributionAlgorithm == service.UnitsAlgorithmID {
		result, err := service.ComputeUnits(unitsContextFrom(parsed))
		if err != nil {
			return nil, err
		}
		return compareUnitsResults(parsed, result), nil
	}
	result, err := service.NewLotteryService().Compute(lotteryContextFrom(parsed))
	if err != nil {
		return nil, err
	}
	return compareResults(parsed, result), nil
}

// waivedPayoutWaitSeconds is how long a payout must have waited before the
// higher fee cap applies: 72 hours, measured between the seed block's median
// time past and the payout block's.
const waivedPayoutWaitSeconds = 72 * 60 * 60

// unitsContextFrom turns a version 5 report into the allocation's input. The
// public inputs come from the header verbatim; the platform's statements are
// carried alongside for the policy checks.
func unitsContextFrom(parsed *parsedCSV) service.UnitsContext {
	h := parsed.header
	return service.UnitsContext{
		Public: service.UnitsPublicInputs{
			Seed:                        h.seed,
			PoolSatoshi:                 h.poolSatoshi,
			PlatformFeeBps:              int64(math.Round(h.platformFeePercentage * 100)),
			UnitFloorSatoshi:            h.unitFloorSatoshi,
			MinUnits:                    h.minUnits,
			OutputCap:                   h.outputCap,
			// The report states percentages with up to two decimals; the
			// allocation takes basis points, so 7.5 becomes 750 exactly.
			FeeBudgetBps:                int64(math.Round(h.feeBudgetPercentage * 100)),
			MaxFeeBps:                   int64(math.Round(h.maxFeePercentage * 100)),
			WaivedMaxFeeBps:             int64(math.Round(h.waivedMaxFeePercentage * 100)),
			TxOverheadVBytes:            int64(h.txOverheadVBytes),
			InputVBytes:                 int64(h.inputP2WSHBytes),
			OutputVBytes:                int64(h.outputDefaultVBytes),
			OpReturnVBytes:              int64(h.opReturnVBytes),
			SeedBlockMedianFeeRateSatVb: h.seedBlockMedianFeeRateSatVb,
			Balances:                    parsed.balances,
		},
		Reported: service.UnitsPlatformReported{
			FeeRateMilliSatPerVb: h.feeRateMilliSatVb,
			FeeRateSource:        h.feeRateSource,
			FeeCapWaived:         h.feeCapWaived,
			LadderRateSource:     h.ladderRateSource,
			OpReturnVariant:      h.opReturnVariant,
		},
	}
}

// checkUnitsPolicy holds the platform's statements in a version 5 report to
// the rules it published. The allocation itself is recomputed from public
// inputs in compareUnitsResults; this is about what the platform chose.
func checkUnitsPolicy(parsed *parsedCSV) []string {
	h := parsed.header
	issues := make([]string, 0)

	// The decimal rate and the exact milli-satoshi rate must be the same
	// figure; the allocation is priced from the exact one.
	if math.Round(h.feeRateDecimal*1000) != float64(h.feeRateMilliSatVb) {
		issues = append(issues, fmt.Sprintf("fee_rate_sat_vb %v and fee_rate_milli_sat_vb %d disagree", h.feeRateDecimal, h.feeRateMilliSatVb))
	}

	switch h.ladderRateSource {
	case service.UnitsLadderRateSeedBlock, service.UnitsLadderRateNodeEstimate:
	default:
		issues = append(issues, fmt.Sprintf("unknown ladder_rate_source %q", h.ladderRateSource))
	}

	if h.poolSatoshi > 0 {
		limit := h.maxFeePercentage
		if h.feeCapWaived {
			limit = h.waivedMaxFeePercentage
		}
		if limit > 0 {
			cap := float64(h.poolSatoshi) * limit / 100
			if float64(parsed.summary.estimatedMinerFeeSatoshi) > cap {
				issues = append(issues, fmt.Sprintf(
					"miner fee %d sats is %.2f%% of the pool, over the %.2f%% cap that applies (waived=%t)",
					parsed.summary.estimatedMinerFeeSatoshi,
					float64(parsed.summary.estimatedMinerFeeSatoshi)/float64(h.poolSatoshi)*100,
					limit, h.feeCapWaived))
			}
		}
	}

	// A waived cap is only legitimate after the wait. Both block times are on
	// the chain, so this is the one part of "we waited" that can be checked
	// offline - once the payout block is known.
	if h.feeCapWaived && h.payoutBlockMTP > 0 {
		if h.seedBlockMTP <= 0 {
			issues = append(issues, "fee cap waived but the report has no seed_block_mtp to measure the wait from")
		} else if h.seedBlockMTP+waivedPayoutWaitSeconds > h.payoutBlockMTP {
			issues = append(issues, fmt.Sprintf(
				"fee cap waived but the payout block's time (%d) is only %d seconds after the seed block's (%d); %d required",
				h.payoutBlockMTP, h.payoutBlockMTP-h.seedBlockMTP, h.seedBlockMTP, waivedPayoutWaitSeconds))
		}
	}

	if h.unitSatoshi < service.UnitsDustLimitSatoshi {
		issues = append(issues, fmt.Sprintf("unit of %d sats is below the %d sat dust limit; such a payout must not have been made",
			h.unitSatoshi, service.UnitsDustLimitSatoshi))
	}

	return issues
}

func compareUnitsResults(parsed *parsedCSV, result *service.UnitsResult) []string {
	issues := make([]string, 0)
	h := parsed.header

	check := func(name string, reported, computed int64) {
		if reported != computed {
			issues = append(issues, fmt.Sprintf("%s mismatch: report=%d computed=%d", name, reported, computed))
		}
	}
	check("platform_fee_satoshi", parsed.summary.platformFeeSatoshi, result.PlatformFeeSatoshi)
	check("estimated_miner_fee_satoshi", parsed.summary.estimatedMinerFeeSatoshi, result.MinerFeeSatoshi)
	check("distributable_satoshi", parsed.summary.distributableSatoshi, result.DistributableSatoshi)
	if parsed.summary.feeRateSatVb != h.feeRateDecimal {
		issues = append(issues, fmt.Sprintf("fee rate disagrees between sections: header=%v result_summary=%v", h.feeRateDecimal, parsed.summary.feeRateSatVb))
	}
	check("unit_step_satoshi", h.unitStepSatoshi, result.UnitStepSatoshi)
	check("unit_satoshi", h.unitSatoshi, result.UnitSatoshi)
	check("unit_count", h.unitCount, result.UnitCount)
	check("extra_units", h.extraUnits, result.ExtraUnits)
	check("max_outputs", h.maxOutputs, result.MaxOutputs)
	check("residual_satoshi", h.residualSatoshi, result.ResidualSatoshi)
	if h.drawOffset != result.DrawOffset {
		issues = append(issues, fmt.Sprintf("draw_offset mismatch: report=%q computed=%q", h.drawOffset, result.DrawOffset))
	}
	if h.residualAddress != result.ResidualAddress {
		issues = append(issues, fmt.Sprintf("residual_address mismatch: report=%q computed=%q", h.residualAddress, result.ResidualAddress))
	}

	issues = append(issues, checkFeePolicy(parsed)...)
	issues = append(issues, checkUnitsPolicy(parsed)...)
	issues = append(issues, checkSettlementBlocks(parsed)...)

	computed := make(map[string]service.UnitsRow, len(result.Rows))
	for _, row := range result.Rows {
		computed[row.Address] = row
	}
	if len(parsed.winners) != len(computed) {
		issues = append(issues, fmt.Sprintf("participant row count mismatch: report=%d computed=%d", len(parsed.winners), len(computed)))
	}
	for _, winner := range parsed.winners {
		row, ok := computed[winner.address]
		if !ok {
			issues = append(issues, fmt.Sprintf("row for %s is not among the computed participants", winner.address))
			continue
		}
		check("holding score for "+winner.address, winner.weight, row.Score)
		check("entitlement_satoshi for "+winner.address, winner.entitlementSatoshi, row.EntitlementSatoshi)
		check("units_guaranteed for "+winner.address, winner.unitsGuaranteed, row.UnitsGuaranteed)
		check("units_extra for "+winner.address, winner.unitsExtra, row.UnitsExtra)
		check("final_reward_satoshi for "+winner.address, winner.finalRewardSatoshi, row.FinalRewardSatoshi)
		if winner.originalRewardSatoshi != winner.finalRewardSatoshi || winner.isDust {
			issues = append(issues, fmt.Sprintf("%s: units_v2 pays no dust and has no redistribution, but the row says original=%d final=%d is_dust=%t",
				winner.address, winner.originalRewardSatoshi, winner.finalRewardSatoshi, winner.isDust))
		}
	}

	return issues
}

// lotteryContextFrom turns a parsed report into the lottery's input.
//
// One function rather than one per caller. The tests replay the draw the same
// way main does, and building the context twice meant a field could be added
// here, missed there, and every test would still pass while checking a
// different computation than the one the tool performs.
func lotteryContextFrom(parsed *parsedCSV) service.LotteryContext {
	return service.LotteryContext{
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
			DataOutputVBytes:      parsed.header.opReturnVBytes,
		},
	}
}

// appendUnique keeps the list of unrecognised row kinds short: a file with ten
// thousand junk rows should name the kind once, not ten thousand times.
func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	if len(list) >= 10 {
		return list
	}
	return append(list, value)
}
