package cloud

import "testing"

func TestEnsureOKRejectsEmptyCode(t *testing.T) {
	cases := map[string]bool{
		`{"code":"0","data":{}}`:        true,
		`{"success":true}`:              true,
		`{"code":"","data":{}}`:         false, // 旧版会误判为成功
		`{"code":"401","message":"no"}`: false,
		`{"data":{}}`:                   false,
		`not json`:                      false,
	}
	for body, wantOK := range cases {
		_, err := ensureOK([]byte(body))
		if (err == nil) != wantOK {
			t.Errorf("ensureOK(%s) err=%v, want ok=%v", body, err, wantOK)
		}
	}
	_, err := ensureOK([]byte(`{"code":"401","message":"no"}`))
	if !IsKind(err, KindAuth) {
		t.Errorf("401 should map to auth kind, got %v", err)
	}
}

func TestPartSize(t *testing.T) {
	if PartSize(1) != minPartSize || PartSize(100<<20) != minPartSize {
		t.Fatal("small files should use the minimum part size")
	}
	big := int64(30) << 30
	ps := PartSize(big)
	if n := (big + ps - 1) / ps; n > maxPartCount {
		t.Fatalf("too many parts: %d", n)
	}
	if ps%(1<<20) != 0 {
		t.Fatal("part size should be MiB aligned")
	}
	parts := planParts(0)
	if len(parts) != 1 || parts[0].PartSize != 0 {
		t.Fatalf("empty file should have one empty part: %+v", parts)
	}
}

func TestParseTime(t *testing.T) {
	for _, in := range []string{`"2026-10-01T08:00:00.000+08:00"`, `1700000000`, `1700000000000`, `"2026-10-01 08:00:00"`} {
		if ts, _ := parseTime([]byte(in)); ts.IsZero() {
			t.Errorf("parseTime(%s) failed", in)
		}
	}
}
