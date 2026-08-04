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
  - `Verification passed. The draw was computed correctly from the scores and
    seed this report states.`
  - followed by the two checks the tool cannot make for you
- Failure:
  - `Verification failed. Possible causes: report file is incorrect or incomplete; tool version mismatch; input parameters are incorrect.`

### 🚨 「通過」是什麼意思，以及還有哪兩件事要你自己做

通過代表**用報告自己列出的積分與種子重算，中獎者與金額跟報告寫的一樣**。

它**沒有**回答另外兩個問題，而且這支工具答不了——它只讀一個檔案、不連任何網路，
那正是任何人都能拿去跑而不必信任它連到哪裡的原因。所以通過之後它會把那兩件事列出來：

**1. 這是不是官方公布的那份檔案**

工具印出所讀檔案的 SHA-256。活動頁面的「驗證包」區塊、CSV 下載按鈕旁邊就寫著官方的值
（API 也會在 `X-CSV-SHA256` 這個 header 回傳同一個值）。**兩者相同才代表檔案沒被動過。**

⚠️ 摘要與 CSV 來自同一個回應，所以它擋的是**傳輸途中或第三方轉載的竄改**，
不是後端自己說謊。

**2. 種子是不是真的是那顆區塊的雜湊**

報告會說種子來自哪個區塊。**去任何區塊瀏覽器查那個高度，比對雜湊。**
工具會把那個高度印出來，但它是**報告自己的說法，工具沒有驗證它**——
把 `seed_block_height` 改成任何數字，驗證一樣會通過，因為抽獎取決於種子的值，
而不是取決於誰說它來自哪個區塊。

### 想確認它真的在重算、不只是在比對檔案？

改一個數字再跑一次。把某個參與者的 `holding_score_sat_blocks` 改大，工具會說：

```
- lottery weight mismatch for bc1qbbb: report=100800000000 computed=999900000000
- original_reward_satoshi mismatch for bc1qbbb: report=17821717 computed=63907475
```

它報告的是「你給的輸入重算出來是這個，跟報告寫的不一樣」——一個只做檔案指紋比對的
工具說不出這種話。

### 它會拒絕看不懂的東西

尾端多出來的位元組、不認得的列，都會被拒絕：一個無法解釋自己讀到什麼的工具，
沒有立場說這個檔案沒問題。

用 Excel 開啟再存檔會加上 BOM，那**不影響驗證**——內容沒有被改動。

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
