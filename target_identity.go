package main

import (
	"crypto/hmac"
	"crypto/sha256"
)

type TargetIdentity struct {
	RequestURL  string
	SafeURL     string
	Fingerprint []byte
}

func newTargetIdentity(secret []byte, normalizedURL string) TargetIdentity {
	return TargetIdentity{
		RequestURL:  normalizedURL,
		SafeURL:     redactURL(normalizedURL),
		Fingerprint: fingerprintURL(secret, normalizedURL),
	}
}

func fingerprintURL(secret []byte, normalizedURL string) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(normalizedURL))
	return mac.Sum(nil)
}
