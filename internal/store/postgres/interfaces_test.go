package postgres_test

import (
	"github.com/julianbonomini/trueseal-relay/internal/notify"
	"github.com/julianbonomini/trueseal-relay/internal/store"
	pgstore "github.com/julianbonomini/trueseal-relay/internal/store/postgres"
)

// Compile-time interface checks.
var _ store.InboxStore = (*pgstore.Store)(nil)
var _ notify.Notifier  = (*pgstore.Store)(nil)
