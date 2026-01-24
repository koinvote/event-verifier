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

type resultSummary struct {
	platformFeeSatoshi       int64
	estimatedMinerFeeSatoshi int64
	distributableSatoshi     int64
	feeRateSatVb             int64
}

type resultWinner struct {
	address               string
	balanceSatoshi        int64
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

const toolVersion = "1.0.0"

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
		if strings.ToLower(strings.TrimSpace(record[0])) == "type" {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(record[0])) {
		case "header":
			if len(record) < 11 {
				return nil, fmt.Errorf("invalid header row length: %d", len(record))
			}
			parsed.header.seed = strings.TrimSpace(record[1])
			parsed.header.poolSatoshi = mustInt64(record[2], "pool_satoshi")
			parsed.header.winnerCount = mustInt(record[3], "winner_count")
			parsed.header.platformFeePercentage = mustFloat(record[4], "platform_fee_percentage")
			parsed.header.dustThresholdSatoshi = mustInt64(record[5], "dust_threshold_satoshi")
			parsed.header.payoutFeeMultiplier = mustFloat(record[6], "payout_fee_multiplier")
			parsed.header.feeRateSatVb = mustInt64(record[7], "fee_rate_sat_vb")
			parsed.header.txOverheadVBytes = mustInt(record[8], "tx_overhead_vbytes")
			parsed.header.inputP2WSHBytes = mustInt(record[9], "input_p2wsh_bytes")
			parsed.header.outputDefaultVBytes = mustInt(record[10], "output_default_vbytes")
			parsed.hasHeader = true
		case "balance":
			if len(record) < 3 {
				return nil, fmt.Errorf("invalid balance row length: %d", len(record))
			}
			parsed.balances = append(parsed.balances, service.BalanceEntry{
				Address: strings.TrimSpace(record[1]),
				Balance: mustInt64(record[2], "balance_satoshi"),
			})
		case "result_summary":
			if len(record) < 5 {
				return nil, fmt.Errorf("invalid result_summary row length: %d", len(record))
			}
			parsed.summary.platformFeeSatoshi = mustInt64(record[1], "platform_fee_satoshi")
			parsed.summary.estimatedMinerFeeSatoshi = mustInt64(record[2], "estimated_miner_fee_satoshi")
			parsed.summary.distributableSatoshi = mustInt64(record[3], "distributable_satoshi")
			parsed.summary.feeRateSatVb = mustInt64(record[4], "fee_rate_sat_vb")
			parsed.hasResult = true
		case "result_winner":
			if len(record) < 6 {
				return nil, fmt.Errorf("invalid result_winner row length: %d", len(record))
			}
			parsed.winners = append(parsed.winners, resultWinner{
				address:               strings.TrimSpace(record[1]),
				balanceSatoshi:        mustInt64(record[2], "balance_satoshi"),
				originalRewardSatoshi: mustInt64(record[3], "original_reward_satoshi"),
				finalRewardSatoshi:    mustInt64(record[4], "final_reward_satoshi"),
				isDust:                mustBool(record[5], "is_dust"),
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

		if winner.balanceSatoshi != computedWinner.Balance {
			issues = append(issues, fmt.Sprintf("balance mismatch for %s: report=%d computed=%d", winner.address, winner.balanceSatoshi, computedWinner.Balance))
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
