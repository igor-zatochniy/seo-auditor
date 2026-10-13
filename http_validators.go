package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

func validatorCipher(key []byte) (cipher.AEAD, error) {
	if len(key) < 32 {
		return nil, errors.New("fingerprint key is required for HTTP validator encryption")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("seo-auditor:http-validator:v1"))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealValidator(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	aead, err := validatorCipher(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(value), nil)
	return "v1:" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func openValidator(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, "v1:") {
		return "", errors.New("unknown HTTP validator format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, "v1:"))
	if err != nil {
		return "", err
	}
	aead, err := validatorCipher(key)
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", errors.New("invalid HTTP validator ciphertext")
	}
	value, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	return string(value), err
}
