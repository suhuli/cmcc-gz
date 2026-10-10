package cloud

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

// 与 Python pc_login.py 相同的密钥（安全项按要求保持不变）。
var (
	transportKey = []byte("UqEZkrjCKfa02pP6jntzFmkzOz86zHUC") // AES-256
	loginDataKey = []byte("qPqDw263XgFgL3u8")                 // AES-128
)

func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, error) {
	if len(b) == 0 || len(b)%blockSize != 0 {
		return nil, errors.New("invalid padded length")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > blockSize || n > len(b) {
		return nil, errors.New("invalid padding")
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, errors.New("invalid padding")
		}
	}
	return b[:len(b)-n], nil
}

// EncryptTransport 对应 Python encrypt_transport：AES-256-CBC，输出 base64(iv || 密文)。
func EncryptTransport(clear string, iv []byte) (string, error) {
	if iv == nil {
		iv = make([]byte, 16)
		if _, err := rand.Read(iv); err != nil {
			return "", err
		}
	}
	if len(iv) != 16 {
		return "", errors.New("IV must be 16 bytes")
	}
	block, err := aes.NewCipher(transportKey)
	if err != nil {
		return "", err
	}
	plain := pkcs7Pad([]byte(clear), aes.BlockSize)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(append(append([]byte{}, iv...), out...)), nil
}

// DecryptTransport 对应 Python decrypt_transport：明文 JSON 原样返回，否则 AES-256-CBC 解密。
func DecryptTransport(raw string) (string, error) {
	text := strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff")
	if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
		return text, nil
	}
	if pad := (4 - len(text)%4) % 4; pad > 0 {
		text += strings.Repeat("=", pad)
	}
	packet, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", err
	}
	if len(packet) <= 16 {
		return "", errors.New("encrypted response is too short")
	}
	block, err := aes.NewCipher(transportKey)
	if err != nil {
		return "", err
	}
	body := packet[16:]
	if len(body)%aes.BlockSize != 0 {
		return "", errors.New("ciphertext is not block aligned")
	}
	plain := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, packet[:16]).CryptBlocks(plain, body)
	unpadded, err := pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}

// DecryptLoginData 对应 Python decrypt_login_data：AES-128-ECB，输入为 hex。
func DecryptLoginData(cipherHex string) (string, error) {
	raw, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(loginDataKey)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return "", errors.New("ciphertext is not block aligned")
	}
	plain := make([]byte, len(raw))
	for i := 0; i < len(raw); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], raw[i:i+aes.BlockSize])
	}
	unpadded, err := pkcs7Unpad(plain, aes.BlockSize)
	if err != nil {
		return "", err
	}
	return string(unpadded), nil
}
