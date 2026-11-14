package desktop

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const KeyringServiceName = "com.vueltario.desktop"

// StoredSession holds desktop session credentials inside the OS Keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service).
// Credentials are NEVER written to plaintext config files (PRD Section 11).
type StoredSession struct {
	ServerURL   string `json:"server_url"`
	AccountID   string `json:"account_id"`
	Email       string `json:"email"`
	AccessToken string `json:"access_token"`
}

type SecretStore interface {
	SaveSession(sess StoredSession) error
	LoadSession(accountKey string) (*StoredSession, error)
	DeleteSession(accountKey string) error
}

type OSKeyringStore struct {
	service string
}

func NewOSKeyringStore() *OSKeyringStore {
	return &OSKeyringStore{service: KeyringServiceName}
}

func (k *OSKeyringStore) SaveSession(sess StoredSession) error {
	key := normalizeAccountKey(sess.Email)
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	return keyring.Set(k.service, key, string(raw))
}

func (k *OSKeyringStore) LoadSession(accountKey string) (*StoredSession, error) {
	raw, err := keyring.Get(k.service, normalizeAccountKey(accountKey))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var sess StoredSession
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (k *OSKeyringStore) DeleteSession(accountKey string) error {
	err := keyring.Delete(k.service, normalizeAccountKey(accountKey))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// MemorySecretStore provides deterministic in-memory secret storage for automated tests and CI environments without a GUI keychain daemon.
type MemorySecretStore struct {
	mu    sync.RWMutex
	items map[string]string
}

func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{items: make(map[string]string)}
}

func (m *MemorySecretStore) SaveSession(sess StoredSession) error {
	raw, err := json.Marshal(sess)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.items[normalizeAccountKey(sess.Email)] = string(raw)
	m.mu.Unlock()
	return nil
}

func (m *MemorySecretStore) LoadSession(accountKey string) (*StoredSession, error) {
	m.mu.RLock()
	raw, ok := m.items[normalizeAccountKey(accountKey)]
	m.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	var sess StoredSession
	if err := json.Unmarshal([]byte(raw), &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (m *MemorySecretStore) DeleteSession(accountKey string) error {
	m.mu.Lock()
	delete(m.items, normalizeAccountKey(accountKey))
	m.mu.Unlock()
	return nil
}

func normalizeAccountKey(k string) string {
	s := strings.ToLower(strings.TrimSpace(k))
	if s == "" {
		return "default"
	}
	return s
}
