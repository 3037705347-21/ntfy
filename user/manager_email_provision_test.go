package user

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"heckel.io/ntfy/v2/db"
	"heckel.io/ntfy/v2/db/pg"
	dbtest "heckel.io/ntfy/v2/db/test"
)

// openManagerFunc opens a manager against the shared test backend and RETURNS the creation
// error, unlike newManagerFunc which fails the test immediately. This is needed to assert
// that conflicting auth-emails configuration prevents manager creation. Multiple opens within
// one test see the same underlying database.
type openManagerFunc func(conf *Config) (*Manager, error)

// forEachEmailBackend runs the test against a fresh SQLite database and, when
// NTFY_TEST_DATABASE_URL is set, a temporary PostgreSQL schema.
func forEachEmailBackend(t *testing.T, f func(t *testing.T, open openManagerFunc)) {
	t.Run("sqlite", func(t *testing.T) {
		dir := t.TempDir()
		f(t, func(conf *Config) (*Manager, error) {
			return NewSQLiteManager(filepath.Join(dir, "user.db"), "", conf)
		})
	})
	t.Run("postgres", func(t *testing.T) {
		schemaDSN := dbtest.CreateTestPostgresSchema(t)
		f(t, func(conf *Config) (*Manager, error) {
			host, err := pg.Open(schemaDSN)
			require.Nil(t, err)
			d := db.New(host, nil)
			m, err := NewPostgresManager(d, conf)
			if err != nil {
				d.Close() // Avoid leaking the pool when the expected provisioning failure aborts startup
			}
			return m, err
		})
	})
}

func mustOpenManager(t *testing.T, open openManagerFunc, conf *Config) *Manager {
	t.Helper()
	a, err := open(conf)
	require.Nil(t, err)
	t.Cleanup(func() { a.Close() })
	return a
}

func reopenManager(t *testing.T, a *Manager, open openManagerFunc, conf *Config) *Manager {
	t.Helper()
	require.Nil(t, a.Close())
	return mustOpenManager(t, open, conf)
}

// emailMap loads the account's verified emails into an address -> Email map.
func emailMap(t *testing.T, a *Manager, userID string) map[string]*Email {
	t.Helper()
	emails, err := a.Emails(userID)
	require.Nil(t, err)
	m := make(map[string]*Email)
	for _, e := range emails {
		m[e.Address] = e
	}
	return m
}

func provisionedEmailConf(username, address string, recovery bool) *ProvisionedEmail {
	return &ProvisionedEmail{Username: username, Address: address, Recovery: recovery}
}

// requireConflict opens the manager with the given config and asserts that startup fails with
// an aggregated ProvisioningConflictError. It returns the conflict for further assertions.
func requireConflict(t *testing.T, open openManagerFunc, conf *Config) *ProvisioningConflictError {
	t.Helper()
	a, err := open(conf)
	require.Error(t, err)
	require.Nil(t, a)
	require.True(t, errors.Is(err, ErrEmailProvisioningConflict), "expected ErrEmailProvisioningConflict, got %v", err)
	var conflict *ProvisioningConflictError
	require.True(t, errors.As(err, &conflict))
	require.NotEmpty(t, conflict.Conflicts, "expected at least one conflict description")
	return conflict
}

// TestManager_ProvisionedEmails_ApplyAndReconcile is the end-to-end reconcile scenario:
// assignments land on both provisioned and self-signed-up accounts, removed managed addresses
// are revoked, self-verified addresses survive, and the recovery (primary) star follows the
// configured address.
func TestManager_ProvisionedEmails_ApplyAndReconcile(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		hash, err := HashPassword("benpass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users:            []*User{{Name: "ben", Hash: hash, Role: RoleUser}},
		}
		a := mustOpenManager(t, open, conf)

		// A regular, self-signed-up account (NOT in auth-users)
		require.Nil(t, a.AddUser("alice", "alice", RoleUser, false))
		// ben verified an address himself before the admin list exists
		ben, err := a.User("ben")
		require.Nil(t, err)
		require.Nil(t, a.AddEmail(ben.ID, "ben-personal@example.com"))
		require.Nil(t, a.SetPrimaryEmail(ben.ID, "ben-personal@example.com"))

		// Second start: the admin list assigns emails to both kinds of accounts
		conf.Emails = []*ProvisionedEmail{
			provisionedEmailConf("ben", "ben@example.com", true),
			provisionedEmailConf("ben", "ben-work@example.com", false),
			provisionedEmailConf("alice", "alice@example.com", true),
		}
		a = reopenManager(t, a, open, conf)

		ben, err = a.User("ben")
		require.Nil(t, err)
		benEmails := emailMap(t, a, ben.ID)
		require.Len(t, benEmails, 3)
		require.True(t, benEmails["ben@example.com"].Primary)
		require.True(t, benEmails["ben@example.com"].Provisioned)
		require.False(t, benEmails["ben-work@example.com"].Primary)
		require.True(t, benEmails["ben-work@example.com"].Provisioned)
		// The self-verified address survives, keeps its flag, but loses the primary star
		self := benEmails["ben-personal@example.com"]
		require.NotNil(t, self)
		require.False(t, self.Provisioned)
		require.False(t, self.Primary)

		// The recovery email works for login and resolves to the right account
		u, err := a.Authenticate("ben@example.com", "benpass")
		require.Nil(t, err)
		require.Equal(t, "ben", u.Name)
		ownerID, err := a.UserIDByPrimaryEmail("ben@example.com")
		require.Nil(t, err)
		require.Equal(t, ben.ID, ownerID)

		// alice (self-signed-up) got her managed emails too
		alice, err := a.User("alice")
		require.Nil(t, err)
		require.False(t, alice.Provisioned)
		aliceEmails := emailMap(t, a, alice.ID)
		require.Len(t, aliceEmails, 1)
		require.True(t, aliceEmails["alice@example.com"].Primary)
		require.True(t, aliceEmails["alice@example.com"].Provisioned)
		u, err = a.Authenticate("alice@example.com", "alice")
		require.Nil(t, err)
		require.Equal(t, "alice", u.Name)

		// Third start: ben-work is delisted, alice disappears from the list entirely
		conf.Emails = []*ProvisionedEmail{
			provisionedEmailConf("ben", "ben@example.com", true),
		}
		a = reopenManager(t, a, open, conf)

		benEmails = emailMap(t, a, ben.ID)
		require.Len(t, benEmails, 2)
		require.True(t, benEmails["ben@example.com"].Primary)
		require.NotNil(t, benEmails["ben-personal@example.com"]) // self-verified survives
		require.Nil(t, benEmails["ben-work@example.com"])        // managed address revoked

		// alice's managed address is swept, but the account itself is untouched
		aliceEmails = emailMap(t, a, alice.ID)
		require.Empty(t, aliceEmails)
		_, err = a.User("alice")
		require.Nil(t, err)

		// Fourth start: the list is gone entirely (slow path, auth-users still present)
		conf.Emails = nil
		a = reopenManager(t, a, open, conf)

		benEmails = emailMap(t, a, ben.ID)
		require.Len(t, benEmails, 1)
		require.NotNil(t, benEmails["ben-personal@example.com"]) // only the self-verified address remains
		primary, err := a.PrimaryEmail(ben.ID)
		require.Nil(t, err)
		require.Equal(t, "", primary)
	})
}

// TestManager_ProvisionedEmails_RecoveryStar covers primary/recovery transitions: moving the
// star, demoting a managed primary when no recovery is configured, and -- crucially -- never
// demoting or revoking a self-verified primary the config never owned.
func TestManager_ProvisionedEmails_RecoveryStar(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		hash, err := HashPassword("benpass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users:            []*User{{Name: "ben", Hash: hash, Role: RoleUser}},
			Emails: []*ProvisionedEmail{
				provisionedEmailConf("ben", "ben@example.com", true),
				provisionedEmailConf("ben", "ben2@example.com", false),
			},
		}
		a := mustOpenManager(t, open, conf)
		ben, err := a.User("ben")
		require.Nil(t, err)

		// Move the recovery star from ben@ to ben2@
		conf.Emails = []*ProvisionedEmail{
			provisionedEmailConf("ben", "ben@example.com", false),
			provisionedEmailConf("ben", "ben2@example.com", true),
		}
		a = reopenManager(t, a, open, conf)
		emails := emailMap(t, a, ben.ID)
		require.True(t, emails["ben2@example.com"].Primary)
		require.False(t, emails["ben@example.com"].Primary)

		// No recovery configured anymore: the managed primary is demoted, nothing deleted
		conf.Emails = []*ProvisionedEmail{
			provisionedEmailConf("ben", "ben@example.com", false),
			provisionedEmailConf("ben", "ben2@example.com", false),
		}
		a = reopenManager(t, a, open, conf)
		emails = emailMap(t, a, ben.ID)
		require.Len(t, emails, 2)
		primary, err := a.PrimaryEmail(ben.ID)
		require.Nil(t, err)
		require.Equal(t, "", primary)

		// ben self-verifies an address and promotes it himself while the list has no recovery
		require.Nil(t, a.AddEmail(ben.ID, "ben-personal@example.com"))
		require.Nil(t, a.SetPrimaryEmail(ben.ID, "ben-personal@example.com"))

		// Another reconcile with no recovery marker must leave the self-verified primary alone
		a = reopenManager(t, a, open, conf)
		emails = emailMap(t, a, ben.ID)
		require.True(t, emails["ben-personal@example.com"].Primary)
		require.False(t, emails["ben-personal@example.com"].Provisioned)

		// Listing the self-verified address as recovery uses it (star stays) without seizing
		// ownership: the row remains non-provisioned.
		conf.Emails = append(conf.Emails, provisionedEmailConf("ben", "ben-personal@example.com", true))
		a = reopenManager(t, a, open, conf)
		emails = emailMap(t, a, ben.ID)
		require.True(t, emails["ben-personal@example.com"].Primary)
		require.False(t, emails["ben-personal@example.com"].Provisioned)

		// Delisting the self-verified address neither revokes nor demotes it -- the config
		// never owned the row.
		conf.Emails = []*ProvisionedEmail{
			provisionedEmailConf("ben", "ben@example.com", false),
			provisionedEmailConf("ben", "ben2@example.com", false),
		}
		a = reopenManager(t, a, open, conf)
		emails = emailMap(t, a, ben.ID)
		require.NotNil(t, emails["ben-personal@example.com"])
		require.True(t, emails["ben-personal@example.com"].Primary)
	})
}

// TestManager_ProvisionedEmails_Conflict_DuplicateAssignment verifies that assigning the same
// address to two accounts rejects the entire list (and the whole provisioning transaction).
func TestManager_ProvisionedEmails_Conflict_DuplicateAssignment(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		hash, err := HashPassword("pass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users: []*User{
				{Name: "ben", Hash: hash, Role: RoleUser},
				{Name: "alice", Hash: hash, Role: RoleUser},
			},
			Emails: []*ProvisionedEmail{
				provisionedEmailConf("ben", "shared@example.com", true),
				provisionedEmailConf("alice", "shared@example.com", false),
			},
		}
		conflict := requireConflict(t, open, conf)
		require.Len(t, conflict.Conflicts, 1)
		msg := conflict.Conflicts[0]
		require.Contains(t, msg, "shared@example.com")
		require.Contains(t, msg, "line 1")
		require.Contains(t, msg, "line 2")
		require.Contains(t, msg, `"ben"`)
		require.Contains(t, msg, `"alice"`)

		// Nothing was applied: even the provisioned users were rolled back
		a := mustOpenManager(t, open, &Config{ProvisionEnabled: true})
		users, err := a.Users()
		require.Nil(t, err)
		require.Len(t, users, 1) // Only the anonymous "*" user
		require.Equal(t, Everyone, users[0].Name)
	})
}

// TestManager_ProvisionedEmails_Conflict_MultipleRecovery verifies that two recovery entries
// for one account reject the list and pinpoint both entries.
func TestManager_ProvisionedEmails_Conflict_MultipleRecovery(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		hash, err := HashPassword("pass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users:            []*User{{Name: "ben", Hash: hash, Role: RoleUser}},
			Emails: []*ProvisionedEmail{
				provisionedEmailConf("ben", "ben@example.com", true),
				provisionedEmailConf("ben", "ben2@example.com", true),
			},
		}
		conflict := requireConflict(t, open, conf)
		require.Len(t, conflict.Conflicts, 1)
		msg := conflict.Conflicts[0]
		require.Contains(t, msg, `"ben"`)
		require.Contains(t, msg, "2 recovery emails")
		require.Contains(t, msg, "ben@example.com")
		require.Contains(t, msg, "ben2@example.com")
	})
}

// TestManager_ProvisionedEmails_Conflict_UnknownAccount verifies that entries targeting a
// non-existent account reject the list.
func TestManager_ProvisionedEmails_Conflict_UnknownAccount(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		conf := &Config{
			ProvisionEnabled: true,
			Emails:           []*ProvisionedEmail{provisionedEmailConf("ghost", "ghost@example.com", true)},
		}
		conflict := requireConflict(t, open, conf)
		require.Len(t, conflict.Conflicts, 1)
		require.Contains(t, conflict.Conflicts[0], `"ghost"`)
		require.Contains(t, conflict.Conflicts[0], "does not exist")
	})
}

// TestManager_ProvisionedEmails_Conflict_RecoveryOwnedElsewhere verifies that making an
// address the recovery email of an account, while it is already another account's recovery
// email, rejects the list instead of hijacking password recovery.
func TestManager_ProvisionedEmails_Conflict_RecoveryOwnedElsewhere(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		// ben self-verified ben@ and made it his primary
		a := mustOpenManager(t, open, &Config{ProvisionEnabled: true})
		require.Nil(t, a.AddUser("ben", "ben", RoleUser, false))
		ben, err := a.User("ben")
		require.Nil(t, err)
		require.Nil(t, a.AddEmail(ben.ID, "ben@example.com"))
		require.Nil(t, a.SetPrimaryEmail(ben.ID, "ben@example.com"))
		require.Nil(t, a.Close())

		// The list tries to assign ben@ as alice's recovery
		hash, err := HashPassword("pass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users:            []*User{{Name: "alice", Hash: hash, Role: RoleUser}},
			Emails:           []*ProvisionedEmail{provisionedEmailConf("alice", "ben@example.com", true)},
		}
		conflict := requireConflict(t, open, conf)
		require.Len(t, conflict.Conflicts, 1)
		msg := conflict.Conflicts[0]
		require.Contains(t, msg, "ben@example.com")
		require.Contains(t, msg, `"alice"`)
		require.Contains(t, msg, `"ben"`)

		// Nothing changed: ben still owns the primary, alice was not created
		a = mustOpenManager(t, open, &Config{ProvisionEnabled: true})
		ownerID, err := a.UserIDByPrimaryEmail("ben@example.com")
		require.Nil(t, err)
		require.Equal(t, ben.ID, ownerID)
		_, err = a.User("alice")
		require.ErrorIs(t, err, ErrUserNotFound)
	})
}

// TestManager_ProvisionedEmails_Conflict_AggregatesAll verifies that multiple independent
// problems are all reported in a single run (the operator fixes everything at once).
func TestManager_ProvisionedEmails_Conflict_AggregatesAll(t *testing.T) {
	forEachEmailBackend(t, func(t *testing.T, open openManagerFunc) {
		hash, err := HashPassword("pass", bcrypt.MinCost)
		require.Nil(t, err)
		conf := &Config{
			ProvisionEnabled: true,
			Users: []*User{
				{Name: "ben", Hash: hash, Role: RoleUser},
				{Name: "alice", Hash: hash, Role: RoleUser},
			},
			Emails: []*ProvisionedEmail{
				{Username: "ghost", Address: "ghost@example.com", Recovery: true}, // unknown account
				{Username: "ben", Address: "not-an-email", Recovery: false},       // invalid address
				provisionedEmailConf("ben", "dup@example.com", false),             // duplicate (line 3)
				provisionedEmailConf("alice", "dup@example.com", false),           // duplicate (line 4)
				provisionedEmailConf("ben", "one@example.com", true),              // 2x recovery on ben
				provisionedEmailConf("ben", "two@example.com", true),              // 2x recovery on ben
			},
		}
		conflict := requireConflict(t, open, conf)
		joined := ""
		for _, c := range conflict.Conflicts {
			joined += c + "\n"
		}
		require.GreaterOrEqual(t, len(conflict.Conflicts), 4)
		require.Contains(t, joined, `"ghost"`)
		require.Contains(t, joined, "not-an-email")
		require.Contains(t, joined, "dup@example.com")
		require.Contains(t, joined, "2 recovery emails")
	})
}
