package service

import "github.com/disaster/dagger-kubernetes/internal/domain"

// LevelSpanIDs returns the base64 span IDs whose logs belong to the level
// rooted at focus (focus itself included). Internal spans (dagger.io/ui.internal
// or name-internal per ci_steps.go) are excluded; their non-internal
// descendants are kept. focus == "" or focus == root.SpanID selects the whole
// trace (returns nil, true). found=false when focus is not present.
func LevelSpanIDs(root *domain.SpanNode, focus string) (ids []string, found bool) {
	if root == nil {
		return nil, focus == ""
	}
	if focus == "" || focus == root.SpanID {
		return nil, true
	}

	var target *domain.SpanNode
	var walk func(n *domain.SpanNode, depth int)
	walk = func(n *domain.SpanNode, depth int) {
		if n == nil || target != nil || depth > ciMaxAbsoluteDepth {
			return
		}
		if n.SpanID == focus {
			target = n
			return
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(root, 0)
	if target == nil {
		return nil, false
	}

	collect(target, 0, &ids)
	return ids, true
}

// collect walks n's subtree appending non-internal span IDs (depth-capped).
func collect(n *domain.SpanNode, depth int, ids *[]string) {
	if n == nil || depth > ciMaxAbsoluteDepth {
		return
	}
	if n.SpanID != "" && !isInternalSpanNode(n) {
		*ids = append(*ids, n.SpanID)
	}
	for _, c := range n.Children {
		collect(c, depth+1, ids)
	}
}

// isInternalSpanNode reports whether n is internal noise (attribute or name).
func isInternalSpanNode(n *domain.SpanNode) bool {
	if n == nil {
		return false
	}
	if n.Attributes["dagger.io/ui.internal"] == "true" {
		return true
	}
	return isInternalSpanName(n.Name)
}
