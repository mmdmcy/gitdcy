package main

import (
	"reflect"
	"testing"
)

// personalAt puts an address together, so this file holds none itself (the
// check reads it too when it is pushed).
func personalAt(local, domain string) string { return local + "@" + domain }

func TestPersonalConsumerAddressesAndWordsAreFoundMasked(t *testing.T) {
	me, someone, proton := personalAt("me", "gmail.com"), personalAt("Someone", "Gmail.com"), personalAt("x", "proton.me")
	allowed := map[string]bool{me: true}
	words := []string{"Jane Doe"}
	got := personalIn("signed in as "+someone+", plan on "+proton+".", words, allowed)
	want := []personalFound{{findingConsumerAddress, "s…@gmail.com"}, {findingConsumerAddress, "x…@proton.me"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got = personalIn("met jane doe gisteren", words, allowed)
	want = []personalFound{{findingPersonalWord, "j… (word 1 of the personal list)"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestPersonalOwnIdentityCompanyAddressesAndHandlesAreNotFound(t *testing.T) {
	me := personalAt("me", "gmail.com")
	allowed := map[string]bool{me: true}
	words := []string{"Jane Doe"}
	for _, line := range []string{
		"author " + me,
		personalAt("info", "example.com") + " and " + personalAt("noreply", "github.com"),
		"a lone @ sign and @handle",
	} {
		if got := personalIn(line, words, allowed); len(got) != 0 {
			t.Errorf("%q: %v", line, got)
		}
	}
	if got := personalIn("nothing here", []string{" "}, allowed); len(got) != 0 {
		t.Errorf("an empty word found: %v", got)
	}
}

func TestPersonalTheSameAddressTwiceOnALineIsOneFinding(t *testing.T) {
	someone := personalAt("someone", "hotmail.com")
	if got := personalIn(someone+" "+someone, nil, map[string]bool{}); len(got) != 1 {
		t.Errorf("got %v", got)
	}
}

func TestPersonalLowercaseIsRusts(t *testing.T) {
	for in, want := range map[string]string{"ΟΔΥΣΣΕΥΣ": "οδυσσευς", "Σ": "σ", "İx": "i̇x", "ABC": "abc"} {
		if got := rustLower(in); got != want {
			t.Errorf("rustLower(%q) = %q, want %q", in, got, want)
		}
	}
}
