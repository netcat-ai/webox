package signedpayload

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

var encoding = base64.RawURLEncoding

func Encode(key string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	payload := encoding.EncodeToString(data)
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(payload))
	return payload + "." + encoding.EncodeToString(mac.Sum(nil)), nil
}

func Decode(key, token string, value any) error {
	payload, signature, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok {
		return errors.New("missing signature")
	}
	want, err := encoding.DecodeString(signature)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(payload))
	if !hmac.Equal(want, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}
	data, err := encoding.DecodeString(payload)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}
