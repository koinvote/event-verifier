package main

import (
	"encoding/csv"
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

type headerRow struct {
	seed                  string
	settlementBlockHeight int64 // absent in reports predating BTC-Time scoring
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
	feeRateSatVb             int64
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
}

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

	ctx := service.LotteryContext{
		Seed:        parsed.header.seed,
		PoolSatoshi: parsed.header.poolSatoshi,
		WinnerCount: parsed.header.winnerCount,
		Balances:    parsed.balances,
		Params: service.LotteryParams{
			PlatformFeePercentage: parsed.header.platformFeePercentage,
			DustThresholdSatoshi:  parsed.header.dustThresholdSatoshi,
			PayoutFeeMultiplier:   parsed.header.payoutFeeMultiplier,
			FeeRateSatVb:          parsed.header.feeRateSatVb,
			TxOverheadVBytes:      parsed.header.txOverheadVBytes,
			InputP2WSHBytes:       parsed.header.inputP2WSHBytes,
			OutputDefaultVBytes:   parsed.header.outputDefaultVBytes,
		},
	}

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
		fmt.Println("Verification passed. The lottery result matches the report.")
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

func loadReport(path string) (*parsedCSV, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open report: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1

	parsed := &parsedCSV{}
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
			// Present only under BTC-Time scoring; older reports simply omit it.
			if raw, ok := cols.lookup(record, "settlement_block_height"); ok && raw != "" {
				parsed.header.settlementBlockHeight = mustInt64(raw, "settlement_block_height")
			}
			parsed.header.poolSatoshi = mustInt64(cols.require(record, "header", "pool_satoshi"), "pool_satoshi")
			parsed.header.winnerCount = mustInt(cols.require(record, "header", "winner_count"), "winner_count")
			parsed.header.platformFeePercentage = mustFloat(cols.require(record, "header", "platform_fee_percentage"), "platform_fee_percentage")
			parsed.header.dustThresholdSatoshi = mustInt64(cols.require(record, "header", "dust_threshold_satoshi"), "dust_threshold_satoshi")
			parsed.header.payoutFeeMultiplier = mustFloat(cols.require(record, "header", "payout_fee_multiplier"), "payout_fee_multiplier")
			parsed.header.feeRateSatVb = mustInt64(cols.require(record, "header", "fee_rate_sat_vb"), "fee_rate_sat_vb")
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
			parsed.summary.feeRateSatVb = mustInt64(cols.require(record, "result_summary", "fee_rate_sat_vb"), "fee_rate_sat_vb")
			parsed.hasResult = true
		case "result_winner":
			parsed.winners = append(parsed.winners, resultWinner{
				address:               cols.require(record, "result_winner", "address"),
				weight:                mustInt64(cols.require(record, "result_winner", "holding_score_sat_blocks", "balance_satoshi"), "holding_score_sat_blocks"),
				originalRewardSatoshi: mustInt64(cols.require(record, "result_winner", "original_reward_satoshi"), "original_reward_satoshi"),
				finalRewardSatoshi:    mustInt64(cols.require(record, "result_winner", "final_reward_satoshi"), "final_reward_satoshi"),
				isDust:                mustBool(cols.require(record, "result_winner", "is_dust"), "is_dust"),
			})
		}
	}

	if !parsed.hasHeader {
		return nil, fmt.Errorf("missing header row")
	}
	if !parsed.hasResult {
		return nil, fmt.Errorf("missing result_summary row")
	}
	if len(parsed.balances) == 0 {
		return nil, fmt.Errorf("missing balance rows")
	}

	return parsed, nil
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
		issues = append(issues, fmt.Sprintf("fee_rate_sat_vb mismatch: report=%d computed=%d", parsed.summary.feeRateSatVb, result.FeeRateSatVb))
	}

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
