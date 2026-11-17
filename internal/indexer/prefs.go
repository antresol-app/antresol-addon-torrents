package indexer

import (
	"context"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
)

// Prefs returns the stored release preferences (defaults when unset; fields
// missing in the stored JSON keep their defaults).
func (m *Manager) Prefs(ctx context.Context) (Prefs, error) {
	return releaseprefs.Store{DB: m.db}.Load(ctx)
}

// SavePrefs validates and stores the preferences.
func (m *Manager) SavePrefs(ctx context.Context, p Prefs) error {
	return releaseprefs.Store{DB: m.db}.Save(ctx, p)
}
