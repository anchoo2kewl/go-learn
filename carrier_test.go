package learn

import "testing"

func TestCarrierFor(t *testing.T) {
	cases := []struct{ word, sentence, want string }{
		{"Elastio", "Welcome to Elastio's platform.", "Welcome to {w}'s platform."},
		{"Elastio", "The elastio-stack deploys it.", "The {w}-stack deploys it."},
		{"Elastio", "", "Today we'll look at {w}, and how it works."},
		{"Elastio", "Nothing here.", "Today we'll look at {w}, and how it works."},
	}
	for _, c := range cases {
		if got := carrierFor(c.word, c.sentence); got != c.want {
			t.Errorf("carrierFor(%q, %q) = %q, want %q", c.word, c.sentence, got, c.want)
		}
	}
}
