package vocab

import "testing"

func TestApply_MatchLiteralAgainstParams(t *testing.T) {
	on, off := true, false

	s, err := Apply(&Overlay{MatchLiteralAgainstParams: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !s.MatchLiteralAgainstParams {
		t.Error("the overlay did not turn the option on")
	}
	if s.Fingerprint() == Default().Fingerprint() {
		t.Error("turning the option on did not change the fingerprint")
	}

	s, err = Apply(&Overlay{MatchLiteralAgainstParams: &off})
	if err != nil {
		t.Fatal(err)
	}
	if s.MatchLiteralAgainstParams || s.Fingerprint() != Default().Fingerprint() {
		t.Error("stating the option off must equal the default, fingerprint included")
	}

	if Default().MatchLiteralAgainstParams {
		t.Error("the option is on by default")
	}
}
