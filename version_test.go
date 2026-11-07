package main

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The README tells people to run --version to learn which report formats
// their build reads, so the three places that state the version must agree:
// the constant the flag prints, the README's header, and the schema the
// parser accepts. A release once shipped with the README at 5.0.0 and the
// binary still printing 4.0.0.
func TestVersionIsStatedConsistently(t *testing.T) {
	major := strings.SplitN(toolVersion, ".", 2)[0]
	if got := strconv.Itoa(maxSupportedSchemaVersion); got != major {
		t.Fatalf("toolVersion %s but maxSupportedSchemaVersion %d", toolVersion, maxSupportedSchemaVersion)
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^Version: (\S+)`).FindSubmatch(readme)
	if m == nil {
		t.Fatal("README has no 'Version:' line")
	}
	if string(m[1]) != toolVersion {
		t.Fatalf("README says %s, binary says %s", m[1], toolVersion)
	}
}
