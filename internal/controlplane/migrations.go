package controlplane

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

type Migration struct {
	Version uint64
	Name    string
	Up      string
	Down    string
}

func (m Migration) Checksum() string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%s", m.Version, m.Name, m.Up, m.Down)))
	return hex.EncodeToString(h.Sum(nil))
}

type MigrationRegistry struct{ items []Migration }

func NewMigrationRegistry(items ...Migration) (MigrationRegistry, error) {
	copyItems := append([]Migration(nil), items...)
	sort.Slice(copyItems, func(i, j int) bool { return copyItems[i].Version < copyItems[j].Version })
	for i, m := range copyItems {
		if m.Version == 0 || m.Name == "" || m.Up == "" {
			return MigrationRegistry{}, fmt.Errorf("invalid migration at index %d", i)
		}
		if i > 0 && copyItems[i-1].Version == m.Version {
			return MigrationRegistry{}, fmt.Errorf("duplicate migration version %d", m.Version)
		}
	}
	return MigrationRegistry{items: copyItems}, nil
}

func (r MigrationRegistry) Pending(applied map[uint64]string) ([]Migration, error) {
	out := make([]Migration, 0, len(r.items))
	for _, m := range r.items {
		if checksum, ok := applied[m.Version]; ok {
			if checksum != m.Checksum() {
				return nil, fmt.Errorf("migration %d checksum mismatch", m.Version)
			}
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (r MigrationRegistry) All() []Migration { return append([]Migration(nil), r.items...) }
