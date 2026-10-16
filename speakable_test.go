package learn

import "testing"

func TestSpeakable(t *testing.T) {
	for in, want := range map[string]string{
		"ee-LAS-tee-oh": "Eelasteeoh",
		"Ehlastyo":      "Ehlastyo",
		"A W S":         "A W S",
		"re-encrypt":    "re-encrypt",
		"Hyper-V":       "Hyper-V",
		"e-LAST-yo":     "Elastyo",
	} {
		if got := speakable(in); got != want {
			t.Errorf("speakable(%q) = %q, want %q", in, got, want)
		}
	}
}
