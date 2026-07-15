package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AccountImportSnapshot struct {
	AccountID     int64
	BatchID       string
	EncryptedJSON string
	ImportedAt    time.Time
	UpdatedAt     time.Time
}

type AccountImportSnapshotRepository interface {
	Upsert(ctx context.Context, snapshot AccountImportSnapshot) error
	GetByAccountID(ctx context.Context, accountID int64) (*AccountImportSnapshot, error)
}

type AccountImportSnapshotView struct {
	Exists     bool      `json:"exists"`
	BatchID    string    `json:"batch_id,omitempty"`
	ImportedAt time.Time `json:"imported_at,omitempty"`
	JSON       any       `json:"json,omitempty"`
}

type AccountImportSnapshotService struct {
	repo      AccountImportSnapshotRepository
	encryptor SecretEncryptor
}

func NewAccountImportSnapshotService(repo AccountImportSnapshotRepository, encryptor SecretEncryptor) *AccountImportSnapshotService {
	return &AccountImportSnapshotService{repo: repo, encryptor: encryptor}
}

func (s *AccountImportSnapshotService) Save(ctx context.Context, accountID int64, batchID string, source any) error {
	if accountID <= 0 || strings.TrimSpace(batchID) == "" {
		return errors.New("account ID and import batch ID are required")
	}
	if s == nil || s.repo == nil || s.encryptor == nil {
		return errors.New("account import snapshot service is unavailable")
	}
	plain, err := json.Marshal(source)
	if err != nil {
		return fmt.Errorf("encode account import snapshot: %w", err)
	}
	encrypted, err := s.encryptor.Encrypt(string(plain))
	if err != nil {
		return fmt.Errorf("encrypt account import snapshot: %w", err)
	}
	now := time.Now().UTC()
	return s.repo.Upsert(ctx, AccountImportSnapshot{
		AccountID: accountID, BatchID: strings.TrimSpace(batchID), EncryptedJSON: encrypted,
		ImportedAt: now, UpdatedAt: now,
	})
}

func (s *AccountImportSnapshotService) GetMasked(ctx context.Context, accountID int64) (*AccountImportSnapshotView, error) {
	view, err := s.load(ctx, accountID)
	if err != nil || !view.Exists {
		return view, err
	}
	view.JSON = MaskImportJSON(view.JSON)
	return view, nil
}

func (s *AccountImportSnapshotService) Reveal(ctx context.Context, accountID int64) (*AccountImportSnapshotView, error) {
	return s.load(ctx, accountID)
}

func (s *AccountImportSnapshotService) load(ctx context.Context, accountID int64) (*AccountImportSnapshotView, error) {
	if accountID <= 0 {
		return nil, errors.New("account ID must be positive")
	}
	if s == nil || s.repo == nil || s.encryptor == nil {
		return nil, errors.New("account import snapshot service is unavailable")
	}
	snapshot, err := s.repo.GetByAccountID(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get account import snapshot: %w", err)
	}
	if snapshot == nil {
		return &AccountImportSnapshotView{Exists: false}, nil
	}
	plain, err := s.encryptor.Decrypt(snapshot.EncryptedJSON)
	if err != nil {
		return nil, fmt.Errorf("decrypt account import snapshot: %w", err)
	}
	var decoded any
	if err := json.Unmarshal([]byte(plain), &decoded); err != nil {
		return nil, fmt.Errorf("decode account import snapshot: %w", err)
	}
	return &AccountImportSnapshotView{
		Exists: true, BatchID: snapshot.BatchID, ImportedAt: snapshot.ImportedAt, JSON: decoded,
	}, nil
}

func MaskImportJSON(value any) any {
	switch current := value.(type) {
	case map[string]any:
		masked := make(map[string]any, len(current))
		for key, item := range current {
			if isSensitiveImportKey(key) {
				masked[key] = maskImportSecret(item)
				continue
			}
			masked[key] = MaskImportJSON(item)
		}
		return masked
	case []any:
		masked := make([]any, len(current))
		for i, item := range current {
			masked[i] = MaskImportJSON(item)
		}
		return masked
	default:
		return value
	}
}

func isSensitiveImportKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	for _, separator := range []string{"-", ".", " ", "/"} {
		normalized = strings.ReplaceAll(normalized, separator, "_")
	}
	compact := strings.ReplaceAll(normalized, "_", "")
	switch compact {
	case "apikey", "accesstoken", "refreshtoken", "idtoken", "sessionkey", "clientsecret":
		return true
	}
	for _, part := range strings.Split(normalized, "_") {
		switch part {
		case "token", "secret", "password", "cookie", "authorization", "session", "key":
			return true
		}
	}
	return false
}

func maskImportSecret(value any) any {
	text, ok := value.(string)
	if !ok || len(text) <= 5 {
		return "***"
	}
	return text[:3] + "***" + text[len(text)-2:]
}
