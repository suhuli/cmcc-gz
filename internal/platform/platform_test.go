package platform

import "testing"

func TestUNCAndMapping(t *testing.T) {
	if got := UNC("127.0.0.1", 8380); got != `\\127.0.0.1@8380\DavWWWRoot` {
		t.Fatal(got)
	}
	if !IsOurMapping(`\\127.0.0.1@8380\DavWWWRoot`, "127.0.0.1", 8380) || !IsOurMapping(`\\127.0.0.1@8380\davwwwroot\`, "127.0.0.1", 8380) {
		t.Fatal("should match")
	}
	if IsOurMapping(`\\127.0.0.1@8381\DavWWWRoot`, "127.0.0.1", 8380) || IsOurMapping(`\\server\share`, "127.0.0.1", 8380) || IsOurMapping("", "127.0.0.1", 8380) || IsOurMapping(`\\127.0.0.1@83801\DavWWWRoot`, "127.0.0.1", 8380) {
		t.Fatal("should not match")
	}
}
