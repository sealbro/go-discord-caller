package dave

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    Impl
		wantErr bool
	}{
		{in: "", want: ImplLibdave},
		{in: "libdave", want: ImplLibdave},
		{in: "golibdave", want: ImplLibdave},
		{in: "godave", want: ImplLibdave},
		{in: "LIBDAVE", want: ImplLibdave},
		{in: "dave-go", want: ImplDaveGo},
		{in: "davego", want: ImplDaveGo},
		{in: "dave_go", want: ImplDaveGo},
		{in: "  Dave-Go  ", want: ImplDaveGo},
		// Unknown values still yield a usable Impl alongside the error, so a
		// caller that only warns keeps a working voice stack.
		{in: "mls", want: Default, wantErr: true},
	}

	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("Parse(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if got != tt.want {
			t.Errorf("Parse(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSessionCreateFuncNeverNil(t *testing.T) {
	// A nil create func would leave disgo without a DAVE session and break
	// voice entirely, so even an out-of-band Impl must return something.
	for _, impl := range []Impl{ImplLibdave, ImplDaveGo, Impl("nonsense")} {
		if SessionCreateFunc(impl) == nil {
			t.Errorf("SessionCreateFunc(%q) = nil", impl)
		}
	}
}
