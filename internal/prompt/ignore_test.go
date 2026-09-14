package prompt

import "testing"

func TestIgnorePolicy(t *testing.T) {
	tests := []struct {
		name         string
		old, current []string
		want         bool
	}{
		{"legacy", nil, nil, true}, {"same", []string{"*.log"}, []string{"*.log"}, true},
		{"added", nil, []string{"*.log"}, false}, {"removed", []string{"*.log"}, nil, false},
		{"changed", []string{"*.log"}, []string{"*.tmp"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchesIgnorePolicy(BuildInitial(nil, "Respond in Chinese.\nUse concise language.", tt.old...), tt.current); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}
