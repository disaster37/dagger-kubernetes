package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckFilterArgv(t *testing.T) {
	valid := []struct{ in, want string }{
		{"go-test=TestGet", "--go-test=TestGet"},
		{"vitest-test-file=src/app.test.ts", "--vitest-test-file=src/app.test.ts"},
		{"go-package=./pkg/...", "--go-package=./pkg/..."},
		{"tag=v1.2.3+build:arm64", "--tag=v1.2.3+build:arm64"},
	}
	for _, tc := range valid {
		got, err := CheckFilterArgv(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CheckFilterArgv(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	invalid := []string{
		"--rm -rf",                     // leading dash
		"",                             // empty
		"=value",                       // missing key
		"go-test=",                     // missing value
		"Go-Test=x",                    // uppercase key
		"go test=x",                    // space
		"9test=x",                      // key must start with a letter
		"go-test=$(id)",                // shell metacharacters
		"go-test=x;rm",                 // shell separator
		"go-test=a\nb",                 // newline
		strings.Repeat("a", 60) + "=x", // key too long
	}
	for _, in := range invalid {
		if _, err := CheckFilterArgv(in); !errors.Is(err, ErrValidation) {
			t.Errorf("CheckFilterArgv(%q) err = %v, want ErrValidation", in, err)
		}
	}
	if _, err := CheckFilterArgv("go-test=" + strings.Repeat("a", 201)); !errors.Is(err, ErrValidation) {
		t.Error("over-long value must be rejected")
	}
}

func TestParseCheckFilterArgv(t *testing.T) {
	if got, err := ParseCheckFilterArgv("--go-test=TestGet"); err != nil || got != "go-test=TestGet" {
		t.Fatalf("ParseCheckFilterArgv = %q, %v", got, err)
	}
	if _, err := ParseCheckFilterArgv("--rm -rf"); !errors.Is(err, ErrValidation) {
		t.Fatal("invalid stored filter must be rejected")
	}
}

func TestNormalizeCheckFilters(t *testing.T) {
	got, err := NormalizeCheckFilters(CheckRunFilters{
		Tests:  []string{"TestGet", "go-test=TestGet", "vitest-test-file=app.test.ts"},
		Files:  []string{"./pkg/..."},
		Checks: []string{"lint"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--go-test=TestGet", "--vitest-test-file=app.test.ts", "--go-package=./pkg/...", "--check=lint"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeCheckFilters = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeCheckFilters = %v, want %v", got, want)
		}
	}
	if empty, err := NormalizeCheckFilters(CheckRunFilters{}); err != nil || len(empty) != 0 {
		t.Fatalf("empty request = %v, %v", empty, err)
	}
	if _, err := NormalizeCheckFilters(CheckRunFilters{Tests: []string{"--rm -rf"}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid filter err = %v, want ErrValidation", err)
	}
}

func TestMergeCheckFilters(t *testing.T) {
	merged, err := MergeCheckFilters([]string{"--go-test=TestGet"}, CheckRunFilters{Files: []string{"./pkg/..."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 2 || merged[0] != "--go-test=TestGet" || merged[1] != "--go-package=./pkg/..." {
		t.Fatalf("merged = %v", merged)
	}
	// Duplicates collapse, and a bare "+"-prefixed stored value is normalized.
	deduped, err := MergeCheckFilters([]string{"--go-test=TestGet", "go-test=TestGet"}, CheckRunFilters{Tests: []string{"TestGet"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(deduped) != 1 || deduped[0] != "--go-test=TestGet" {
		t.Fatalf("dedupe failed: %v", deduped)
	}
	if _, err := MergeCheckFilters([]string{"garbage"}, CheckRunFilters{}); !errors.Is(err, ErrValidation) {
		t.Fatal("invalid stored filter must fail validation")
	}
}
