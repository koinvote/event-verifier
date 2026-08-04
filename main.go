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
const maxSupportedSchemaVersion = 3

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

const toolVersion = "2.0.0"

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

	ctx := lotteryContextFrom(parsed)

	lottery := service.NewLotteryService()
	result, err := lottery.Compute(ctx)
	if err != nil {
		printVerificationFailed("input parameters are incorrect")
		if *verbose {
			fmt.Printf("Failed to compute lottery result: %s\n", err)
		}
		os.Exit(1)
	}

	issues := compareResults(parsed, result)
	if len(issues) == 0 {
		fmt.Println("Verification passed. The draw was computed correctly from the")
		fmt.Println("scores and seed this report states.")
		printWhatIsStillYours(parsed)
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
			parsed.winners = append(parsed.winners, resultWinner{
				address:               cols.require(record, "result_winner", "address"),
				weight:                mustInt64(cols.require(record, "result_winner", "holding_score_sat_blocks", "balance_satoshi"), "holding_score_sat_blocks"),
				originalRewardSatoshi: mustInt64(cols.require(record, "result_winner", "original_reward_satoshi"), "original_reward_satoshi"),
				finalRewardSatoshi:    mustInt64(cols.require(record, "result_winner", "final_reward_satoshi"), "final_reward_satoshi"),
				isDust:                mustBool(cols.require(record, "result_winner", "is_dust"), "is_dust"),
			})

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

	if parsed.header.maxFeePercentage > 0 && parsed.header.poolSatoshi > 0 {
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

func printVerificationFailed(reason string) {
	fmt.Println("Verification failed.")
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
// printWhatIsStillYours states the two checks this tool cannot make.
//
// Everything below the "passed" line used to be printed as though it had been
// verified. It had not. The block heights and hashes are read straight out of
// the report and echoed back, so a report that named the wrong block was passed
// and its claim repeated underneath the word "passed" - changing
// seed_block_height to any number at all still verifies, because the draw
// depends on the seed value and not on which block is said to have produced it.
//
// The tool cannot check them: it reads one file and touches no network, which is
// what lets anyone run it without trusting anything it connects to. So they are
// presented as the reader's remaining work rather than as findings, and the
// values are labelled as the report's claims.
func printWhatIsStillYours(parsed *parsedCSV) {
	h := parsed.header

	fmt.Println()
	fmt.Println("Two things this tool cannot check for you:")
	fmt.Println()
	fmt.Println("1. Whether this is the file that was published.")
	fmt.Printf("   SHA-256 of the file read: %s\n", parsed.sha256)
	fmt.Println("   Compare it with the digest shown next to the download link.")
	fmt.Println()
	fmt.Println("2. Whether the seed is really that block's hash.")
	fmt.Printf("   The report says the seed is %s\n", h.seed)
	switch {
	case h.seedBlockHeight > 0:
		fmt.Printf("   and that it came from block %d, with scoring ending at block %d.\n",
			h.seedBlockHeight, h.scoreBlockHeight)
		fmt.Println("   Look that block up in any block explorer and compare the hash.")
		fmt.Println("   Those heights are the report's own claim; nothing here verified them.")
	case h.settlementBlockHeight > 0:
		// Version 1 and 2: one block did both jobs.
		fmt.Printf("   and that it came from block %d, which also ended the scoring.\n",
			h.settlementBlockHeight)
		fmt.Println("   Look that block up in any block explorer and compare the hash.")
		fmt.Println("   That height is the report's own claim; nothing here verified it.")
	default:
		// Nothing to look up, so do not send the reader looking. Searching a
		// block explorer for the hash itself still works and is the only route
		// left; saying so beats a sentence about a block that was never named.
		fmt.Println("   This report does not name the block it came from.")
		fmt.Println("   Search that hash in a block explorer to see whether it is a real block.")
	}
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
