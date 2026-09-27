package main

import "testing"

func TestValidateListen(t *testing.T) {
	for _, tc := range []struct {
		address, identity string
		valid             bool
	}{
		{"127.0.0.1:8097", "", true},
		{"[::1]:8097", "owner@example.com", true},
		{"127.0.0.1:8097", "owner@example.com", true},
		{"100.124.20.20:8097", "", true},
		{"192.168.1.20:8097", "", true},
		{"[fd7a:115c:a1e0::1]:8097", "", true},
		{"100.124.20.20:8097", "owner@example.com", false},
		{"[fd7a:115c:a1e0::1]:8097", "owner@example.com", false},
		{"0.0.0.0:8097", "", false},
		{"[::]:8097", "", false},
		{":8097", "", false},
		{"224.0.0.1:8097", "", false},
		{"255.255.255.255:8097", "", false},
		{"localhost:8097", "", false},
		{"100.124.20.20", "", false},
		{"100.124.20.20:0", "", false},
		{"100.124.20.20:65536", "", false},
		{"100.124.20.20:http", "", false},
	} {
		t.Run(tc.address+"/"+tc.identity, func(t *testing.T) {
			err := validateListen(tc.address, tc.identity)
			if (err == nil) != tc.valid {
				t.Fatalf("validateListen(%q, %q) = %v; want valid=%v", tc.address, tc.identity, err, tc.valid)
			}
		})
	}
}
