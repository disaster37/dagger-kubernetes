package service

import (
	"testing"

	"github.com/disaster/dagger-kubernetes/internal/domain"
)

func TestLevelSpanIDs(t *testing.T) {
	// root
	//  ├── build (visible)
	//  │    ├── GET /query (internal by name)
	//  │    │    └── compile (visible, kept)
	//  │    └── test (visible)
	//  └── passthrough (transparent, kept)
	//       └── lint (visible)
	compile := span("compile", "internal", "compile", "success", nil)
	internal := span("internal", "build", "GET /query", "success", nil, compile)
	test := span("test", "build", "test", "success", nil)
	build := span("build", "root", "build", "success", nil, internal, test)
	lint := span("lint", "pass", "lint", "success", nil)
	passthrough := span("pass", "root", "passthrough", "success", map[string]string{"dagger.io/ui.passthrough": "true"}, lint)
	root := span("root", "", "root", "success", nil, build, passthrough)

	tests := []struct {
		name      string
		root      *domain.SpanNode
		focus     string
		wantIDs   []string
		wantFound bool
	}{
		{
			name:      "empty focus selects whole trace",
			root:      root,
			focus:     "",
			wantIDs:   nil,
			wantFound: true,
		},
		{
			name:      "root focus selects whole trace",
			root:      root,
			focus:     "root",
			wantIDs:   nil,
			wantFound: true,
		},
		{
			name:      "leaf focus",
			root:      root,
			focus:     "test",
			wantIDs:   []string{"test"},
			wantFound: true,
		},
		{
			name:      "subtree focus includes descendants",
			root:      root,
			focus:     "build",
			wantIDs:   []string{"build", "compile", "test"},
			wantFound: true,
		},
		{
			name:      "internal span excluded but non-internal child kept",
			root:      root,
			focus:     "internal",
			wantIDs:   []string{"compile"},
			wantFound: true,
		},
		{
			name:      "passthrough span kept",
			root:      root,
			focus:     "pass",
			wantIDs:   []string{"pass", "lint"},
			wantFound: true,
		},
		{
			name:      "missing focus",
			root:      root,
			focus:     "nope",
			wantIDs:   nil,
			wantFound: false,
		},
		{
			name:      "nil root with empty focus",
			root:      nil,
			focus:     "",
			wantIDs:   nil,
			wantFound: true,
		},
		{
			name:      "nil root with focus",
			root:      nil,
			focus:     "root",
			wantIDs:   nil,
			wantFound: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ids, found := LevelSpanIDs(tc.root, tc.focus)
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if len(ids) != len(tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			for i := range ids {
				if ids[i] != tc.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
				}
			}
		})
	}
}

func TestLevelSpanIDsInternalAttribute(t *testing.T) {
	child := span("child", "internal", "child", "success", nil)
	internal := span("internal", "root", "some-name", "success", map[string]string{"dagger.io/ui.internal": "true"}, child)
	root := span("root", "", "root", "success", nil, internal)

	ids, found := LevelSpanIDs(root, "internal")
	if !found {
		t.Fatal("found = false, want true")
	}
	if len(ids) != 1 || ids[0] != "child" {
		t.Fatalf("ids = %v, want [child]", ids)
	}
}

func TestLevelSpanIDsSkipsEmptySpanID(t *testing.T) {
	child := span("child", "", "child", "success", nil)
	empty := span("", "parent", "empty", "success", nil, child)
	parent := span("parent", "root", "parent", "success", nil, empty)
	root := span("root", "", "root", "success", nil, parent)

	ids, found := LevelSpanIDs(root, "parent")
	if !found {
		t.Fatal("found = false, want true")
	}
	// The empty-ID node is skipped but its child is still collected.
	if len(ids) != 2 || ids[0] != "parent" || ids[1] != "child" {
		t.Fatalf("ids = %v, want [parent child]", ids)
	}
}

func TestLevelSpanIDsDeepTree(t *testing.T) {
	// A chain just under the depth cap is fully walkable.
	leaf := span("leaf", "n", "leaf", "success", nil)
	node := leaf
	for i := 0; i < ciMaxAbsoluteDepth-2; i++ {
		node = span("n", "n", "n", "success", nil, node)
	}
	root := span("root", "", "root", "success", nil, node)

	ids, found := LevelSpanIDs(root, "leaf")
	if !found {
		t.Fatal("found = false for deep leaf, want true")
	}
	if len(ids) != 1 || ids[0] != "leaf" {
		t.Fatalf("ids = %v, want [leaf]", ids)
	}

	// A chain deeper than the cap must terminate without finding the leaf
	// (the walk is depth-capped to bound recursion).
	deepLeaf := span("deep-leaf", "n", "deep-leaf", "success", nil)
	deepNode := deepLeaf
	for i := 0; i < ciMaxAbsoluteDepth+10; i++ {
		deepNode = span("n", "n", "n", "success", nil, deepNode)
	}
	deepRoot := span("root", "", "root", "success", nil, deepNode)

	if _, found := LevelSpanIDs(deepRoot, "deep-leaf"); found {
		t.Fatal("found = true for leaf beyond the depth cap, want false")
	}
}
