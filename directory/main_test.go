package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.yml")
	os.WriteFile(path, []byte(`account_id: x
tunnels:
  pc:
    id: a
    routes:
      llm.chiq.me: http://localhost:8080
  macmini-production:
    id: b
    caddy: true
    routes:
      ssh.chiq.me: ssh://localhost:22
      var.chiq.me: http://127.0.0.1:9998
      crm.example.com:
        service: http://localhost:3070
        caddy: http://localhost:3072
        note: twenty
`), 0o644)

	groups, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Tunnel != "macmini-production" {
		t.Fatalf("macmini should come first: %+v", groups)
	}
	rows := groups[0].Rows
	want := []row{
		{Host: "crm.example.com", Scheme: "http", Port: "3070", LAN: "http://192.168.1.1:3070/", Caddy: "3072", Note: "twenty"},
		{Host: "ssh.chiq.me", Scheme: "ssh", Port: "22"},
		{Host: "var.chiq.me", Scheme: "http", Port: "9998", LAN: "http://192.168.1.1:9998/"}, // Caddy's LAN door
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
	if got := groups[1].Rows[0].LAN; got != "http://192.168.1.3:8080/" {
		t.Errorf("pc LAN link = %q", got)
	}
}
