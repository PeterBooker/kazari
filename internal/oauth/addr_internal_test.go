package oauth

import "testing"

func TestPublicAddr(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"8.8.8.8", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fd00::1", false},
		{"0.0.0.0", false},
		{"0.1.2.3", false},
		{"100.64.0.1", false},
		{"100.100.100.100", false},
		{"192.0.0.170", false},
		{"198.18.0.1", false},
		{"240.0.0.1", false},
		{"255.255.255.255", false},
		{"224.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:10.0.0.1", false},
		{"64:ff9b::a00:1", false},
		{"2002:a00:1::1", false},
		{"fe80::1%eth0", false},
		{"not-an-ip", false},
	}
	for _, tt := range tests {
		if got := publicAddr(tt.host); got != tt.want {
			t.Errorf("publicAddr(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
