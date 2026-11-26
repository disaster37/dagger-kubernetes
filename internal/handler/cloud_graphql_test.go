package handler

import "testing"

// TestGraphQLResponseHasErrors covers the log_queries error flag.
func TestGraphQLResponseHasErrors(t *testing.T) {
	if graphqlResponseHasErrors([]byte(`{"data":{}}`)) {
		t.Error("data-only response must not report errors")
	}
	if !graphqlResponseHasErrors([]byte(`{"errors":[{"message":"x"}]}`)) {
		t.Error("errors response must report errors")
	}
	if !graphqlResponseHasErrors([]byte(`not json`)) {
		t.Error("undecodable response must report errors")
	}
}
