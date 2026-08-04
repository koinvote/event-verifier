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

// Everything under "Verification passed" used to read as though it had been
// checked. The block heights had not been: they are read from the report and
// echoed, so a report naming the wrong block passed and had its claim repeated
// under the word "passed". The tool cannot check them - it reads one file and
// touches no network, which is what lets anyone run it without trusting
// whatever it would otherwise connect to - so the output has to say so.
func TestSeedBlockHeightIsNotVerified(t *testing.T) {
	body := string(bundledReport(t))
	// example-v3 names block 900106 as the seed's origin.
	if !strings.Contains(body, ",900106,") {
		t.Fatal("fixture no longer contains the seed block height this test edits")
	}
	tampered := strings.Replace(body, ",900106,", ",987654,", 1)

	parsed, err := loadReport(writeTemp(t, "wrong-block.csv", []byte(tampered)))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	// Documented, not endorsed: the draw depends on the seed value, not on
	// which block is said to have produced it, so nothing here can tell.
	if parsed.header.seedBlockHeight != 987654 {
		t.Fatalf("seed block height = %d, want the report's claim to be carried through unchanged",
			parsed.header.seedBlockHeight)
	}
}

// The success output has to name both checks the reader still owns. Losing
// either one silently is the failure this wording exists to prevent, and a
// wording change is exactly the kind of edit that would do it.
func TestSuccessOutputNamesBothRemainingChecks(t *testing.T) {
	parsed, err := loadReport("example-v3.csv")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}

	out := captureStdout(t, func() { printWhatIsStillYours(parsed) })

	for _, want := range []string{
		"cannot check for you",
		"published",                  // check 1: is this the published file
		parsed.sha256,                // ...with the digest to compare
		"block explorer",             // check 2: is the seed a real block hash
		"nothing here verified them", // and that the heights are unverified
	} {
		if !strings.Contains(out, want) {
			t.Errorf("success output is missing %q:\n%s", want, out)
		}
	}
}

// A report that names no block must not send the reader looking for one.
func TestOutputDoesNotPointAtABlockTheReportNeverNamed(t *testing.T) {
	parsed, err := loadReport("example-legacy.csv")
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if parsed.header.seedBlockHeight > 0 || parsed.header.settlementBlockHeight > 0 {
		t.Skip("this fixture now names a block, so it cannot cover the missing case")
	}

	out := captureStdout(t, func() { printWhatIsStillYours(parsed) })

	if strings.Contains(out, "Look that block up") {
		t.Errorf("told the reader to look up a block the report never named:\n%s", out)
	}
	if !strings.Contains(out, "does not name the block") {
		t.Errorf("output should say the block is absent:\n%s", out)
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
