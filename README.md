# Event Verifier

Version: 1.0.0

This tool verifies that a payout report CSV matches the official lottery calculation.

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
- Failure:
  - `Verification failed. Possible causes: report file is incorrect or incomplete; tool version mismatch; input parameters are incorrect.`

For detailed differences, add `--verbose`:
```
verify-event.exe --report <your-report-file.csv> --verbose
```
