package service

import "testing"

func TestVerifyRunToken(t *testing.T) {
	token, hash := newRunToken()
	otherToken, otherHash := newRunToken()
	if token == "" || hash == "" || token == otherToken || hash == otherHash {
		t.Fatal("run tokens must be unique and non-empty")
	}
	tests := []struct {
		name  string
		hash  string
		token string
		want  bool
	}{
		{name: "valid", hash: hash, token: token, want: true},
		{name: "missing token", hash: hash, token: "", want: false},
		{name: "missing hash", hash: "", token: token, want: false},
		{name: "wrong token", hash: hash, token: otherToken, want: false},
		{name: "token from another run", hash: otherHash, token: token, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyRunToken(tt.hash, tt.token); got != tt.want {
				t.Fatalf("VerifyRunToken() = %v, want %v", got, tt.want)
			}
		})
	}
}
