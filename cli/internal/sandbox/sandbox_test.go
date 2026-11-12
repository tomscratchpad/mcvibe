package sandbox

import "testing"

func TestOwnerSlug(t *testing.T) {
	for in, want := range map[string]string{
		"alice@corp.com":                 "alice",
		"system:admin":                   "admin",
		"oidc:Bob.Smith@corp.com":        "bob-smith",
		"a-very-long-user-name-indeed-x": "a-very-long-user-nam",
		"@@@":                            "anon",
	} {
		if got := OwnerSlug(in); got != want {
			t.Errorf("OwnerSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "feature-x", "pr-1234"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-x", "x-", "Upper", "a_b", "a.b", "this-name-is-too-long-for-hc-xx"} {
		if ValidateName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
