package main

import (
	"os"
	"path/filepath"
	"testing"
)

// This tool is downloaded and run by people checking a payout for themselves,
// against a file they were handed. The file is therefore untrusted input, and a
// panic on a malformed one is not a crash in a batch job somewhere - it is the
// verification tool falling over in front of the person trying to use it, which
// looks a great deal like the payout being unverifiable.
//
// Every outcome except a panic is acceptable here. Refusing to read a file is a
// perfectly good answer; so is reading it and reporting mismatches. What must
// never happen is that a report claims to have been checked when the parse went
// sideways.
func FuzzLoadReportSurvivesAnyFile(f *testing.F) {
	for _, path := range []string{"example-v2.csv", "example.csv", "example-legacy.csv"} {
		content, err := os.ReadFile(path)
		if err != nil {
			f.Fatalf("read seed %s: %v", path, err)
		}
		f.Add(string(content))
	}

	// Shapes that a truncated download or a hand-edited file would produce.
	f.Add("")
	f.Add("type\n")
	f.Add("header\n")
	f.Add("type,seed\nheader\n")
	f.Add("type,schema_version\nheader,999\n")
	f.Add("type,schema_version\nheader,notanumber\n")
	f.Add("balance,bc1q,,1,2,3\n")
	f.Add("\"unterminated quote")

	f.Fuzz(func(t *testing.T, content string) {
		path := filepath.Join(t.TempDir(), "report.csv")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		parsed, err := loadReport(path)
		if err != nil {
			return
		}
		if parsed == nil {
			t.Fatal("returned no error and no report")
		}

		// A report that parsed must be one this build actually understands.
		// Reading a newer format with older rules is the failure mode this
		// version check exists to prevent, and it must not be reachable by
		// getting the rest of the file to parse.
		if parsed.header.schemaVersion > maxSupportedSchemaVersion {
			t.Fatalf("accepted schema version %d, above the supported %d",
				parsed.header.schemaVersion, maxSupportedSchemaVersion)
		}

		// The policy audit runs on whatever survived parsing, so it has to
		// tolerate it without panicking too.
		_ = checkFeePolicy(parsed)
	})
}
