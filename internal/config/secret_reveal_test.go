package config

import "testing"

func TestListenIsLoopback(t *testing.T) {
	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:8080", true},
		{"127.0.0.1", true},
		{"localhost:8080", true},
		{"LOCALHOST:8080", true},
		{"[::1]:8080", true},
		{"::1", true},
		{"127.5.6.7:9000", true},
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"[::]:8080", false},
		{"192.168.1.10:8080", false},
		{"gateway.example.com:443", false},
		{"", false},
		{"   ", false},
	}
	for _, tc := range cases {
		if got := ListenIsLoopback(tc.listen); got != tc.want {
			t.Errorf("ListenIsLoopback(%q) = %v, want %v", tc.listen, got, tc.want)
		}
	}
}

func TestSecretRevealAllowedDefaultsToLoopbackOnly(t *testing.T) {
	loopback := Config{Listen: "127.0.0.1:8080"}
	if !loopback.SecretRevealAllowed() {
		t.Fatal("loopback listener must default to reveal allowed")
	}
	public := Config{Listen: "0.0.0.0:8080"}
	if public.SecretRevealAllowed() {
		t.Fatal("non-loopback listener must default to reveal denied")
	}
}

func TestSecretRevealExplicitSettingWins(t *testing.T) {
	yes, no := true, false
	loopbackOff := Config{Listen: "127.0.0.1:8080"}
	loopbackOff.Admin.AllowSecretReveal = &no
	if loopbackOff.SecretRevealAllowed() {
		t.Fatal("explicit false must disable reveal even on loopback")
	}
	publicOn := Config{Listen: "0.0.0.0:8080"}
	publicOn.Admin.AllowSecretReveal = &yes
	if !publicOn.SecretRevealAllowed() {
		t.Fatal("explicit true must enable reveal even on a non-loopback listener")
	}
}
