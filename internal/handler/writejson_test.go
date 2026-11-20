package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/route"
)

// TestWriteJSONNilCollections verifies that writeJSON emits `[]`/`{}` for a nil
// slice/map (so list endpoints never return JSON null) while a nil value of any
// other type still encodes as null.
func TestWriteJSONNilCollections(t *testing.T) {
	var nilSlice []string
	var nilMap map[string]string
	var nilPtr *int

	cases := []struct {
		name string
		body any
		want string
	}{
		{name: "nil string slice", body: nilSlice, want: "[]"},
		{name: "empty string slice", body: []string{}, want: "[]"},
		{name: "nil map", body: nilMap, want: "{}"},
		{name: "nil pointer", body: nilPtr, want: "null"},
		{name: "nil interface", body: nil, want: "null"},
		{name: "non-empty slice", body: []string{"a"}, want: `["a"]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			eng := route.NewEngine(config.NewOptions(nil))
			eng.GET("/v", func(_ context.Context, c *app.RequestContext) { writeJSON(c, body) })

			resp := ut.PerformRequest(eng, http.MethodGet, "/v", nil)
			if resp.Result().StatusCode() != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.Result().StatusCode())
			}
			if got := string(resp.Result().Body()); got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
		})
	}
}
