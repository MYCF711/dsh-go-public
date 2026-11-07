package session

import "testing"

func TestNormalizeID(t *testing.T) {
	cases := map[string]string{
		"session-abc": "abc",
		"abc":         "abc",
		"":            "",
		// 只认 "session-" 前缀，别的一概不动
		"session":    "session",
		"SESSion-ab": "SESSion-ab",
	}
	for in, want := range cases {
		if got := NormalizeID(in); got != want {
			t.Fatalf("NormalizeID(%q) = %q，要 %q", in, got, want)
		}
	}
}

func TestCandidateIDs(t *testing.T) {
	if got := CandidateIDs(""); got != nil {
		t.Fatalf("空串要 nil，拿到 %v", got)
	}
	got := CandidateIDs("session-x")
	if len(got) != 2 || got[0] != "x" || got[1] != "session-x" {
		t.Fatalf("带前缀要先试去前缀的，拿到 %v", got)
	}
	got = CandidateIDs("x")
	if len(got) != 1 || got[0] != "x" {
		t.Fatalf("裸 id 不该重复添加，拿到 %v", got)
	}
}
