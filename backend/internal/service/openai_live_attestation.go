package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const liveAttestationHeader = "x-oai-attestation"

const maxLiveAttestationBytes = 16 * 1024

type liveAttestationEnvelope struct {
	Version int    `json:"v"`
	Status  int    `json:"s"`
	Token   string `json:"t"`
}

type liveAttestationAES struct {
	key [32]byte
}

func newLiveAttestationCipher(cfg *config.Config) SecretEncryptor {
	if cfg == nil || strings.TrimSpace(cfg.JWT.Secret) == "" {
		return nil
	}
	return &liveAttestationAES{
		key: sha256.Sum256([]byte("sub2api/live-attestation/v1\x00" + cfg.JWT.Secret)),
	}
}

func (c *liveAttestationAES) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate Live attestation nonce: %w", err)
	}
	encrypted := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(encrypted), nil
}

func (c *liveAttestationAES) Decrypt(ciphertext string) (string, error) {
	encrypted, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode Live attestation: %w", err)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(encrypted) < gcm.NonceSize() {
		return "", errors.New("encrypted Live attestation is too short")
	}
	plaintext, err := gcm.Open(nil, encrypted[:gcm.NonceSize()], encrypted[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt Live attestation: %w", err)
	}
	return string(plaintext), nil
}

func normalizeClientLiveAttestation(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > maxLiveAttestationBytes {
		return "", errors.New("x-oai-attestation is too large")
	}
	var envelope liveAttestationEnvelope
	if err := json.Unmarshal([]byte(value), &envelope); err != nil {
		return "", errors.New("x-oai-attestation must be valid JSON")
	}
	if envelope.Version != 1 || envelope.Status != 0 || !strings.HasPrefix(envelope.Token, "v1.") {
		return "", errors.New("x-oai-attestation has an unsupported format")
	}
	encoded := strings.TrimPrefix(envelope.Token, "v1.")
	if encoded == "" {
		return "", errors.New("x-oai-attestation has an empty token")
	}
	if _, err := base64.RawURLEncoding.DecodeString(encoded); err != nil {
		return "", errors.New("x-oai-attestation token is not valid base64url")
	}
	normalized, err := json.Marshal(envelope)
	if err != nil {
		return "", errors.New("x-oai-attestation cannot be normalized")
	}
	return string(normalized), nil
}

func (s *OpenAIGatewayService) prepareLiveAttestation(ctx context.Context, clientHeader string) (string, string, error) {
	if s == nil || s.liveAttestationCipher == nil {
		return "", "", &LiveAttestationUnavailableError{
			Reason: "JWT secret is required to protect the Sideband attestation",
		}
	}
	header, err := normalizeClientLiveAttestation(clientHeader)
	if err != nil {
		return "", "", err
	}
	if header == "" {
		if s == nil || s.liveAttestation == nil {
			return "", "", &LiveAttestationUnavailableError{
				Reason: "Sub2API has no platform DeviceCheck provider and the client supplied no attestation",
			}
		}
		header, err = s.liveAttestation.Generate(ctx)
		if err != nil {
			return "", "", &LiveAttestationUnavailableError{Reason: err.Error()}
		}
	}
	ciphertext, err := s.liveAttestationCipher.Encrypt(header)
	if err != nil {
		return "", "", &LiveAttestationUnavailableError{
			Reason: "failed to protect the generated DeviceCheck attestation",
		}
	}
	return header, ciphertext, nil
}

func (s *OpenAIGatewayService) decryptLiveAttestation(record *LiveCallRecord) (string, error) {
	if record == nil || strings.TrimSpace(record.AttestationCiphertext) == "" || s.liveAttestationCipher == nil {
		return "", &LiveAttestationUnavailableError{
			Reason: "the Live call has no reusable DeviceCheck attestation",
		}
	}
	header, err := s.liveAttestationCipher.Decrypt(record.AttestationCiphertext)
	if err != nil {
		return "", &LiveAttestationUnavailableError{
			Reason: "the Live call DeviceCheck attestation cannot be decrypted on this instance",
		}
	}
	return header, nil
}
