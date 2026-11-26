package service

import (
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestDeriveTests(t *testing.T) {
	cases := []struct {
		name  string
		root  *domain.SpanNode
		want  []domain.TestResult
		check func(t *testing.T, root *domain.SpanNode)
	}{
		{
			name: "nil root",
		},
		{
			name: "non-test spans ignored",
			root: &domain.SpanNode{SpanID: "a", Name: "build", Status: "success"},
		},
		{
			name: "otel test attributes",
			root: &domain.SpanNode{
				SpanID: "root", Name: "go test", Status: "success",
				Children: []*domain.SpanNode{
					{SpanID: "t1", Name: "TestGet", Status: "success", DurationMS: 42,
						Attributes: map[string]string{
							"test.case.name":          "TestGet",
							"test.suite.name":         "example.com/pkg",
							"test.case.result.status": "pass",
						}},
					{SpanID: "t2", Name: "TestFail", Status: "failed",
						Attributes: map[string]string{
							"test.case.name":             "TestFail",
							"test.case.result.status":    "fail",
							"test.case.file":             "pkg/thing_test.go",
							"test.case.duration_ms":      "17",
							"dagger.io/dag.cached":       "false",
							"test.case.result.status.v2": "ignored",
						}},
					{SpanID: "t3", Name: "TestSkip", Status: "success",
						Attributes: map[string]string{
							"test.case.name":          "TestSkip",
							"test.case.result.status": "skip",
						}},
				},
			},
			want: []domain.TestResult{
				{Name: "TestGet", Suite: "example.com/pkg", Status: "passed", DurationMS: 42, SpanID: "t1"},
				{Name: "TestFail", File: "pkg/thing_test.go", Status: "failed", DurationMS: 17, SpanID: "t2"},
				{Name: "TestSkip", Status: "skipped", SpanID: "t3"},
			},
			check: func(t *testing.T, root *domain.SpanNode) {
				t.Helper()
				if root.Children[0].Test == nil || root.Children[0].Test.Name != "TestGet" {
					t.Fatalf("span Test annotation missing: %+v", root.Children[0].Test)
				}
				if root.Test != nil {
					t.Fatal("non-test root must stay nil")
				}
			},
		},
		{
			name: "engine check attributes",
			root: &domain.SpanNode{
				SpanID: "root", Name: "checks",
				Children: []*domain.SpanNode{
					{SpanID: "c1", Name: "lint", Status: "success",
						Attributes: map[string]string{"dagger.io/check.name": "lint", "dagger.io/check.passed": "true"}},
					{SpanID: "c2", Name: "vet", Status: "success",
						Attributes: map[string]string{"dagger.io/check.name": "vet", "dagger.io/check.passed": "false"}},
				},
			},
			want: []domain.TestResult{
				{Name: "lint", Status: "passed", SpanID: "c1"},
				{Name: "vet", Status: "failed", SpanID: "c2"},
			},
		},
		{
			name: "missing status falls back to span status",
			root: &domain.SpanNode{
				SpanID: "root", Name: "tests",
				Children: []*domain.SpanNode{
					{SpanID: "s1", Name: "one", Status: "success", Attributes: map[string]string{"test.case.name": "one"}},
					{SpanID: "s2", Name: "two", Status: "failed", Attributes: map[string]string{"test.case.name": "two"}},
					{SpanID: "s3", Name: "three", Status: "running", Attributes: map[string]string{"test.case.name": "three"}},
				},
			},
			want: []domain.TestResult{
				{Name: "one", Status: "passed", SpanID: "s1"},
				{Name: "two", Status: "failed", SpanID: "s2"},
				{Name: "three", Status: "running", SpanID: "s3"},
			},
		},
		{
			name: "duplicate span ids emitted once",
			root: &domain.SpanNode{
				SpanID: "root", Name: "tests",
				Children: []*domain.SpanNode{
					{SpanID: "dup", Name: "one", Status: "success", Attributes: map[string]string{"test.case.name": "one"}},
					{SpanID: "dup", Name: "one", Status: "success", Attributes: map[string]string{"test.case.name": "one"}},
				},
			},
			want: []domain.TestResult{{Name: "one", Status: "passed", SpanID: "dup"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveTests(tc.root)
			if len(got) != len(tc.want) {
				t.Fatalf("DeriveTests = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("test[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
			if tc.check != nil {
				tc.check(t, tc.root)
			}
		})
	}
}

func TestMapTestStatus(t *testing.T) {
	cases := []struct{ raw, span, want string }{
		{"pass", "", "passed"},
		{"PASSED", "", "passed"},
		{"true", "", "passed"},
		{"fail", "", "failed"},
		{"false", "", "failed"},
		{"skip", "", "skipped"},
		{"skipped", "", "skipped"},
		{"", "success", "passed"},
		{"", "failed", "failed"},
		{"", "running", "running"},
		{"unknown", "success", "passed"},
	}
	for _, tc := range cases {
		if got := mapTestStatus(tc.raw, tc.span); got != tc.want {
			t.Errorf("mapTestStatus(%q, %q) = %q, want %q", tc.raw, tc.span, got, tc.want)
		}
	}
}

func TestSpanDurationMS(t *testing.T) {
	if got := spanDurationMS(&domain.SpanNode{DurationMS: 9}); got != 9 {
		t.Errorf("span duration fallback = %d, want 9", got)
	}
	if got := spanDurationMS(&domain.SpanNode{DurationMS: 9, Attributes: map[string]string{"test.case.duration_ms": "31"}}); got != 31 {
		t.Errorf("attribute duration = %d, want 31", got)
	}
	if got := spanDurationMS(&domain.SpanNode{DurationMS: 9, Attributes: map[string]string{"test.case.duration_ms": "bogus"}}); got != 9 {
		t.Errorf("invalid attribute must fall back to span duration, got %d", got)
	}
}
