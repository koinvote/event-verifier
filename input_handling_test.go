package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This tool is what somebody runs when they want to check for themselves that a
// payout was fair. That makes how it handles a file it was not expecting part of
// what it promises: refusing a good file, accepting a modified one, or hanging
// with no output all undermine it in ways a wrong number never would, because
// the person running it has no other way to tell.

func bundledReport(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("example-v3.csv")
	if err != nil {
		t.Fatalf("reading the bundled report: %v", err)
	}
	return body
}

func writeTemp(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// Opening a report in Excel and saving it prepends a UTF-8 byte order mark. The
// content is untouched, so the answer has to be the same - reporting a
// verification failure would tell somebody their payout was wrong because their
// spreadsheet program added three bytes.
func TestReportWithExcelByteOrderMarkStillVerifies(t *testing.T) {
	body := bundledReport(t)
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, body...)

	parsed, err := loadReport(writeTemp(t, "bom.csv", withBOM))
	if err != nil {
		t.Fatalf("a byte order mark must not make a report unreadable: %v", err)
	}
	if !parsed.hasHeader || !parsed.hasResult {
		t.Error("the report parsed but came out incomplete")
	}
}

// Bytes appended to a valid report used to be read as rows of an unknown kind
// and dropped, so the file still verified - while its digest no longer matched
// what was published. A tool that cannot account for what it read must not say
// the file checks out.
func TestTrailingBytesAreRejected(t *testing.T) {
	tampered := append(bundledReport(t), 0xFF, 0xFE, 0xFD)

	_, err := loadReport(writeTemp(t, "tampered.csv", tampered))
	if err == nil {
		t.Fatal("expected a report with appended bytes to be refused")
	}
	if !strings.Contains(err.Error(), "does not understand") {
		t.Errorf("error should say what it could not account for, got: %v", err)
	}
	// Quoted rather than printed raw: the value came out of the file and can be
	// any bytes at all.
	if !strings.Contains(err.Error(), `"\xff\xfe\xfd"`) {
		t.Errorf("the unrecognised value should be quoted, got: %v", err)
	}
}

func TestUnknownRowKindIsRejected(t *testing.T) {
	body := string(bundledReport(t)) + "\nsomething_new,1,2,3\n"

	_, err := loadReport(writeTemp(t, "extra.csv", []byte(body)))
	if err == nil {
		t.Fatal("expected an unrecognised row kind to be refused")
	}
	if !strings.Contains(err.Error(), "something_new") {
		t.Errorf("error should name the row kind, got: %v", err)
	}
}

// The same kind repeated must not produce a message as long as the file.
func TestManyUnknownRowsAreSummarised(t *testing.T) {
	body := string(bundledReport(t))
	for i := 0; i < 5000; i++ {
		body += "\njunk,1,2,3"
	}

	_, err := loadReport(writeTemp(t, "junk.csv", []byte(body)))
	if err == nil {
		t.Fatal("expected the file to be refused")
	}
	if strings.Count(err.Error(), "junk") != 1 {
		t.Errorf("the kind should be named once, got: %v", err)
	}
}

// Before this, `--report /dev/zero` never returned: the tool appeared to hang
// with no output at all. A named pipe or an enormous file did the same.
func TestUnboundedInputIsRefusedRatherThanReadForever(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("/dev/zero is not available here")
	}

	done := make(chan error, 1)
	go func() {
		_, err := loadReport("/dev/zero")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an endless file to be refused")
		}
		if !strings.Contains(err.Error(), "larger than") {
			t.Errorf("error should say the file is too large, got: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("loadReport did not return - the size limit is not being applied")
	}
}

// "Verification passed" says the arithmetic in this file is self-consistent. It
// cannot say the file is the published one, because the tool does not know what
// was published - so it prints the digest and leaves that comparison to the
// person who does.
func TestReportDigestIsOfTheBytesOnDisk(t *testing.T) {
	body := bundledReport(t)
	path := writeTemp(t, "report.csv", body)

	parsed, err := loadReport(path)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	want := sha256.Sum256(body)
	if parsed.sha256 != hex.EncodeToString(want[:]) {
		t.Errorf("digest = %s, want %s", parsed.sha256, hex.EncodeToString(want[:]))
	}
}

// The digest has to be of what was on disk, not of what was parsed, or a file
// that differs only by a byte order mark would report the same digest as one
// without - and the digest exists precisely to distinguish files.
func TestDigestDistinguishesAFileWithAByteOrderMark(t *testing.T) {
	body := bundledReport(t)

	plain, err := loadReport(writeTemp(t, "plain.csv", body))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	withBOM, err := loadReport(writeTemp(t, "bom.csv", append([]byte{0xEF, 0xBB, 0xBF}, body...)))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	if plain.sha256 == withBOM.sha256 {
		t.Error("two different files reported the same digest")
	}
}

// The two settlement heights were printed and never questioned. Their distance
// is the one thing about them this tool can check without touching a chain, and
// it is not decoration: fixing the weights six blocks before the seed exists is
// what stops the miner of the seed block from seeing what their hash would pay
// out. A report naming a different pair is describing a settlement that did not
// follow that rule.
func TestSettlementBlockGapIsChecked(t *testing.T) {
	body := string(bundledReport(t))
	if !strings.Contains(body, ",900106,") {
		t.Fatal("fixture no longer contains the seed block height this test edits")
	}

	tests := []struct {
		name       string
		seedHeight string
		wantIssue  bool
	}{
		{name: "correct gap", seedHeight: "900106", wantIssue: false},
		{name: "one block short", seedHeight: "900105", wantIssue: true},
		{name: "one block long", seedHeight: "900107", wantIssue: true},
		{name: "wildly wrong", seedHeight: "987654", wantIssue: true},
		{name: "seed before scoring ended", seedHeight: "900094", wantIssue: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edited := strings.Replace(body, ",900106,", ","+tt.seedHeight+",", 1)
			parsed, err := loadReport(writeTemp(t, "gap.csv", []byte(edited)))
			if err != nil {
				t.Fatalf("loading: %v", err)
			}

			issues := checkSettlementBlocks(parsed)
			if tt.wantIssue && len(issues) == 0 {
				t.Errorf("gap of %d was accepted", parsed.header.seedBlockHeight-parsed.header.scoreBlockHeight)
			}
			if !tt.wantIssue && len(issues) != 0 {
				t.Errorf("correct gap was rejected: %v", issues)
			}
		})
	}
}

// Version 1 and 2 settle on one block that does both jobs, so there is no
// distance to check and demanding one would reject every older report.
func TestSettlementBlockGapIsNotDemandedOfOlderReports(t *testing.T) {
	parsed, err := loadReport("example-legacy.csv")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if parsed.header.seedBlockHeight > 0 && parsed.header.scoreBlockHeight > 0 {
		t.Skip("this fixture now carries two blocks, so it cannot cover the older shape")
	}

	if issues := checkSettlementBlocks(parsed); len(issues) != 0 {
		t.Errorf("a report with one settlement block must not be asked for a gap: %v", issues)
	}
}

// Both lines earn their place by being something the reader can act on. The
// scoring block height was neither - no explorer can say whether a height is
// the right cutoff - and it is checked above rather than recited.
func TestPassingOutputOnlyNamesActionableChecks(t *testing.T) {
	parsed, err := loadReport("example-v3.csv")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	out := captureStdout(t, func() { printRemainingChecks(parsed) })

	for _, want := range []string{"block explorer", parsed.sha256, "download link"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "scoring ended") {
		t.Errorf("the scoring block is checked, not recited:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = saved

	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("reading captured output: %v", err)
	}
	return buf.String()
}
