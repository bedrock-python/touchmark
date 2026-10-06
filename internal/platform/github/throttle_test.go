package github

import "testing"

// A GraphQL query is a read though it is a POST; only a mutation spends
// the write budget: the driver tells the throttle which
// one it sends.
func TestIsMutation(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{mutationUpdateRefs, true},
		{queryOpenPRs, false},
		{queryClosers, false},
		{querySweep, false},
		{batchFilesQuery(3), false},
		{"  # a comment\n\tmutation { x }", true},
		{"{ viewer { login } }", false},
		{"mutations", false},
		{"", false},
	} {
		if got := isMutation(tc.doc); got != tc.want {
			t.Errorf("isMutation(%.40q) = %v, want %v", tc.doc, got, tc.want)
		}
	}
}
