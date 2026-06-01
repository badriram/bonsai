package libvirt

import "testing"

func TestParseFirstIPv4(t *testing.T) {
	// Mirrors real `virsh domifaddr <name>` output. Tabs and column widths
	// vary across libvirt versions; we rely on whitespace tokens, not
	// fixed columns.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "default NAT lease",
			in: ` Name       MAC address          Protocol     Address
-------------------------------------------------------------------------------
 vnet0      52:54:00:12:34:56    ipv4         192.168.122.42/24
`,
			want: "192.168.122.42",
		},
		{
			name: "multiple ifaces — first wins",
			in: ` vnet0  52:54:00:aa:bb:cc  ipv4  10.0.0.5/24
 vnet1  52:54:00:dd:ee:ff  ipv4  192.168.1.10/24
`,
			want: "10.0.0.5",
		},
		{
			name: "ipv6-only ignored",
			in: ` vnet0  52:54:00:aa:bb:cc  ipv6  fe80::abcd/64
`,
			want: "",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseFirstIPv4(tc.in); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestRenderServerScript_emitsRawShellShebang(t *testing.T) {
	out, err := renderServerScript(serverVars{K3sVersion: "v1.31.0+k3s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[:11]; got != "#!/bin/sh\n#" {
		t.Fatalf("first-boot script does not start with shebang: %q", got)
	}
	if !contains(out, "INSTALL_K3S_VERSION=\"v1.31.0+k3s1\"") {
		t.Fatalf("template didn't interpolate K3sVersion: %s", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
