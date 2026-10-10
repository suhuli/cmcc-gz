package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyCompatAndUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	legacy := "\xef\xbb\xbf" + `{"account":{"phone":"13800000000","token":"tk","token_expire_ms":1,"serverinfo":{"a":1},"ext_info":{"profile":"pc"}},
	 "mount":{"drive":"Y:","port":9000},"log_level":"DEBUG","future_field":{"x":[1,2]}}`
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Get()
	if c.Account.Phone != "13800000000" || c.Mount.Drive != "Y:" || c.Mount.Port != 9000 || c.Account.Ext("profile") != "pc" {
		t.Fatalf("parsed = %+v", c)
	}
	if c.Mount.Host != "127.0.0.1" || c.PanelPort != 8390 {
		t.Fatalf("defaults not applied: %+v", c.Mount)
	}
	if !c.Account.Expired(time.Now()) || !c.Account.LoggedIn() {
		t.Fatal("account flags")
	}
	if err := s.Update(func(c *Config) error { c.AutoMount = true; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "future_field") {
		t.Fatal("unknown field dropped")
	}
	s2, err := Open(p)
	if err != nil || !s2.Get().AutoMount {
		t.Fatal("reload failed")
	}
}

func TestCorruptConfigIsBackedUp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().Mount.Drive != "Z:" {
		t.Fatal("defaults expected")
	}
	if _, err := os.Stat(p + ".bad"); err != nil {
		t.Fatal("backup missing")
	}
}

func TestCloneIsDeep(t *testing.T) {
	c := Default()
	c.Account.ExtInfo["k"] = "v"
	d := c.Clone()
	d.Account.ExtInfo["k"] = "changed"
	if c.Account.ExtInfo["k"] != "v" {
		t.Fatal("clone shares map")
	}
}

func TestValidDrive(t *testing.T) {
	for d, want := range map[string]bool{"Z:": true, "D:": true, "C:": false, "Z": false, "ZZ": false, "z:": false} {
		if ValidDrive(d) != want {
			t.Errorf("ValidDrive(%q) != %v", d, want)
		}
	}
	if len(RandomString(24)) != 24 || RandomString(8) == RandomString(8) {
		t.Fatal("RandomString")
	}
}
