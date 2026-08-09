package pipeline

import (
	"testing"
)

func TestComputeVersion_Push(t *testing.T) {
	s := VersionStrategy{
		BranchPattern: "0.0.0-rc.{build}",
		PRPattern:     "0.0.0-pr.{pr}.{build}",
		TagPattern:    "{tag}",
	}
	ctx := VersionContext{
		Event: "push",
		Build: 42,
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.0.0-rc.42" {
		t.Errorf("expected '0.0.0-rc.42', got '%s'", v)
	}
}

func TestComputeVersion_PR(t *testing.T) {
	s := VersionStrategy{
		PRPattern: "0.0.0-pr.{pr}.{build}",
	}
	ctx := VersionContext{
		Event:    "pull_request",
		Build:    42,
		PRNumber: 7,
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.0.0-pr.7.42" {
		t.Errorf("expected '0.0.0-pr.7.42', got '%s'", v)
	}
}

func TestComputeVersion_Tag(t *testing.T) {
	s := VersionStrategy{
		TagPattern: "{tag}",
	}
	ctx := VersionContext{
		Event: "tag",
		Tag:   "v1.2.3",
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "v1.2.3" {
		t.Errorf("expected 'v1.2.3', got '%s'", v)
	}
}

func TestComputeVersion_Release(t *testing.T) {
	s := VersionStrategy{
		BranchPattern: "0.0.0-rc.{build}",
	}
	ctx := VersionContext{
		Event: "release",
		Build: 5,
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.0.0-rc.5" {
		t.Errorf("expected '0.0.0-rc.5', got '%s'", v)
	}
}

func TestComputeVersion_UnknownEvent(t *testing.T) {
	s := VersionStrategy{}
	ctx := VersionContext{
		Event: "unknown",
	}
	_, err := ComputeVersion(s, ctx)
	if err == nil {
		t.Fatal("expected error for unknown event")
	}
}

func TestComputeVersion_PrereleaseSuffix(t *testing.T) {
	s := VersionStrategy{
		BranchPattern:    "0.0.0-rc.{build}",
		PrereleaseSuffix: "-alpha",
	}
	ctx := VersionContext{
		Event: "push",
		Build: 1,
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.0.0-rc.1-alpha" {
		t.Errorf("expected '0.0.0-rc.1-alpha', got '%s'", v)
	}
}

func TestComputeVersion_CustomPattern(t *testing.T) {
	s := VersionStrategy{
		BranchPattern: "1.0.0-dev.{build}",
	}
	ctx := VersionContext{
		Event: "push",
		Build: 99,
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "1.0.0-dev.99" {
		t.Errorf("expected '1.0.0-dev.99', got '%s'", v)
	}
}

func TestComputeVersion_DefaultsApplied(t *testing.T) {
	// Simulating defaults being applied by Validate
	s := VersionStrategy{
		BranchPattern: "0.0.0-rc.{build}",
		PRPattern:     "0.0.0-pr.{pr}.{build}",
		TagPattern:    "{tag}",
	}
	ctx := VersionContext{
		Event: "tag",
		Tag:   "2.0.0",
	}
	v, err := ComputeVersion(s, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "2.0.0" {
		t.Errorf("expected '2.0.0', got '%s'", v)
	}
}
