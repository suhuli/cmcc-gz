package cloud

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type vectors struct {
	Sign []struct {
		Body      string `json:"body"`
		Timestamp string `json:"timestamp"`
		Nonce     string `json:"nonce"`
		Encoded   string `json:"encoded"`
		Signature string `json:"signature"`
	} `json:"sign"`
	Transport []struct {
		Clear string `json:"clear"`
		IVHex string `json:"iv_hex"`
		Wire  string `json:"wire"`
	} `json:"transport"`
	LoginData struct {
		Plain string `json:"plain"`
		Hex   string `json:"hex"`
	} `json:"login_data"`
	BasicAuth struct {
		Kind    string `json:"kind"`
		Account string `json:"account"`
		Token   string `json:"token"`
		Expect  string `json:"expect"`
	} `json:"basic_auth"`
	DeviceInfo struct {
		Expect string `json:"expect"`
	} `json:"device_info"`
	ClientInfo struct {
		Expect string `json:"expect"`
	} `json:"client_info"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignatureMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Sign {
		if got := pyQuote(c.Body); got != c.Encoded {
			t.Errorf("pyQuote(%q)\n got %s\nwant %s", c.Body, got, c.Encoded)
		}
		if got := CreateSignature(c.Body, c.Timestamp, c.Nonce); got != c.Signature {
			t.Errorf("signature(%q) got %s want %s", c.Body, got, c.Signature)
		}
	}
}

func TestTransportMatchesPython(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Transport {
		iv, _ := hex.DecodeString(c.IVHex)
		got, err := EncryptTransport(c.Clear, iv)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.Wire {
			t.Errorf("encrypt mismatch for %q", c.Clear)
		}
		back, err := DecryptTransport(c.Wire)
		if err != nil || back != c.Clear {
			t.Errorf("decrypt round trip failed: %v %q", err, back)
		}
	}
}

func TestLoginDataDecrypt(t *testing.T) {
	v := loadVectors(t)
	got, err := DecryptLoginData(v.LoginData.Hex)
	if err != nil || got != v.LoginData.Plain {
		t.Fatalf("login data: %v %q", err, got)
	}
}

func TestBasicAuthAndDeviceInfo(t *testing.T) {
	v := loadVectors(t)
	if got := basicAuth(v.BasicAuth.Kind, v.BasicAuth.Account, v.BasicAuth.Token); got != v.BasicAuth.Expect {
		t.Errorf("basic auth got %s want %s", got, v.BasicAuth.Expect)
	}
	if got := webDeviceInfo("web-0063"); got != v.DeviceInfo.Expect {
		t.Errorf("device info got %s want %s", got, v.DeviceInfo.Expect)
	}
	want := webDeviceInfo("web-0063") + base64.StdEncoding.EncodeToString([]byte("mobile")) + "||"
	if got := webClientInfo("web-0063"); got != want || got != v.ClientInfo.Expect {
		t.Errorf("client info mismatch")
	}
}

func TestDecryptPlainJSONPassthrough(t *testing.T) {
	in := `{"a":1}`
	out, err := DecryptTransport(in)
	if err != nil || !bytes.Equal([]byte(out), []byte(in)) {
		t.Fatalf("passthrough failed: %v %q", err, out)
	}
}
