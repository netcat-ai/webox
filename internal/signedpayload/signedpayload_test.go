package signedpayload

import "testing"

type testPayload struct {
	Value string `json:"value"`
}

func TestRoundTripAndRejectTampering(t *testing.T) {
	token, err := Encode("secret", testPayload{Value: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded testPayload
	if err := Decode("secret", token, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Value != "hello" {
		t.Fatalf("unexpected payload: %#v", decoded)
	}
	tampered := []byte(token)
	if tampered[0] == 'a' {
		tampered[0] = 'b'
	} else {
		tampered[0] = 'a'
	}
	if err := Decode("secret", string(tampered), &decoded); err == nil {
		t.Fatal("tampered token accepted")
	}
}
