# Event Verifier

Version: 2.0.0

This tool verifies that a payout report CSV matches the official lottery calculation.
It recomputes the draw from the report's own inputs and compares every winner,
reward and fee against what the report claims.

## Which reports this version reads

Version 2.0.0 reads **both** the current reports and every earlier one:

- **BTC-Time scoring** — the lottery weight is `holding_score_sat_blocks`, a
  satoshi x blocks figure that credits how much was held *and for how long*.
  These reports also carry `settlement_block_height`, and per address an
  `average_holding_satoshi` and `join_block_height`.
- **Older reports**, where the weight was `balance_satoshi`: a point-in-time
  balance snapshot. `example-legacy.csv` is one of these, and still verifies.

Columns are matched by **name**, taken from the `type,...` row that precedes
each section, so a report that gains a column stays verifiable.

> **If you are upgrading from 1.0.0, please re-run any report that failed.**
> 1.0.0 read columns by fixed position. When the report gained a
> `referral_code` column, every field after it shifted by one, and 1.0.0
> reported correct payouts as unverifiable. That was a defect in this tool,
> not evidence of anything wrong with the payouts it rejected.

## Usage

1. Clone the repository
```
git clone https://github.com/koinvote/event-verifier.git
cd event-verifier
```

2. Build

Windows (CMD or PowerShell):
```
go build -o verify-event.exe main.go
```

macOS / Linux:
```
go build -o verify-event main.go
```

3. Run verification

Windows:
```
verify-event.exe --report <your-report-file.csv>
```

macOS / Linux:
```
./verify-event --report <your-report-file.csv>
```

## Output

- Success:
  - `Verification passed. The lottery result matches the report.`
  - `SHA-256 of the file verified: <digest>`
- Failure:
  - `Verification failed. Possible causes: report file is incorrect or incomplete; tool version mismatch; input parameters are incorrect.`

### 🚨 「通過」是什麼意思，以及它不保證什麼

通過代表**這個檔案裡的數字彼此一致**：用同樣的種子與同樣的積分重算，中獎者與金額
跟報告寫的一樣。

它**不代表這就是官方公布的那一份檔案**——這支工具無從得知公布的版本長什麼樣。
所以它會印出所讀檔案的 SHA-256，**請自行拿它跟公布的值比對**。兩件事都成立才是完整的驗證。

工具會拒絕它看不懂的內容（尾端多出來的位元組、不認得的列）：一個無法解釋自己讀到什麼的
工具，沒有立場說這個檔案沒問題。用 Excel 開啟再存檔會加上 BOM，那**不影響驗證**，
因為內容沒有被改動。

Exit status is `0` when verification passes and `1` when it does not, so the
tool can be used in a script.

**Always add `--verbose` before drawing any conclusion from a failure.** The
summary above cannot tell the three causes apart; the detail can. In
particular, a line like

```
result_winner row has no original_reward_satoshi column (tool may be older than the report)
```

means this tool is out of date, not that the payout is wrong. Pull the latest
version and run it again.

```
verify-event.exe --report <your-report-file.csv> --verbose
```

## Checking it against a report you trust

Two sample reports are included, and both should pass:

```
./verify-event --report example.csv          # current, BTC-Time scoring
./verify-event --report example-legacy.csv   # pre-BTC-Time balance snapshot
```

If either fails, the build is wrong — stop and report it, rather than
concluding anything about your own report.
