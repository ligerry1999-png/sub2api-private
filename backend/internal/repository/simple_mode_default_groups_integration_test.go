//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func resetSimpleModeDefaultGroupsFixture(t *testing.T, client *dbent.Client, ctx context.Context) {
	t.Helper()

	// The repository integration package shares one database across tests.
	// Rebuild the minimal post-migration state inside this test's transaction so
	// prior tests cannot make a fresh-install case look initialized.
	_, err := client.ExecContext(ctx, "TRUNCATE users, accounts, groups RESTART IDENTITY CASCADE")
	require.NoError(t, err)
	_, err = client.ExecContext(ctx, `
		INSERT INTO groups (name, description, created_at, updated_at)
		VALUES ('default', 'Default group', NOW(), NOW())
	`)
	require.NoError(t, err)
}

func TestEnsureSimpleModeDefaultGroups_CreatesMissingDefaults(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	assertGroupExists := func(name string) {
		exists, err := client.Group.Query().Where(group.NameEQ(name), group.DeletedAtIsNil()).Exist(seedCtx)
		require.NoError(t, err)
		require.True(t, exists, "expected group %s to exist", name)
	}

	assertGroupExists(service.PlatformAnthropic + "-default")
	assertGroupExists(service.PlatformOpenAI + "-default")
	assertGroupExists(service.PlatformGemini + "-default")
	assertGroupExists(service.PlatformAntigravity + "-default-1")
	assertGroupExists(service.PlatformAntigravity + "-default-2")

	grokDefault, err := client.Group.Query().
		Where(group.NameEQ(service.PlatformGrok+"-default"), group.DeletedAtIsNil()).
		Only(seedCtx)
	require.NoError(t, err)
	require.True(t, grokDefault.AllowImageGeneration)
}

func TestEnsureSimpleModeDefaultGroups_PreservesExistingInstallation(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	mustCreateGroup(t, client, &service.Group{
		Name:     "operator-openai-" + time.Now().Format(time.RFC3339Nano),
		Platform: service.PlatformOpenAI,
	})

	beforeCount, err := client.Group.Query().Count(seedCtx)
	require.NoError(t, err)
	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	afterCount, err := client.Group.Query().Count(seedCtx)
	require.NoError(t, err)
	require.Equal(t, beforeCount, afterCount, "an existing installation must not receive new default groups")

	defaultNames := []string{
		service.PlatformAnthropic + "-default",
		service.PlatformOpenAI + "-default",
		service.PlatformGemini + "-default",
		service.PlatformGrok + "-default",
		service.PlatformAntigravity + "-default-1",
		service.PlatformAntigravity + "-default-2",
	}
	for _, name := range defaultNames {
		exists, err := client.Group.Query().Where(group.NameEQ(name), group.DeletedAtIsNil()).Exist(seedCtx)
		require.NoError(t, err)
		require.False(t, exists, "existing installation unexpectedly received group %s", name)
	}
}

func TestEnsureSimpleModeDefaultGroups_PreservesInitializedLegacySeed(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	mustCreateUser(t, client, &service.User{})

	beforeCount, err := client.Group.Query().Count(seedCtx)
	require.NoError(t, err)
	require.Equal(t, 1, beforeCount, "fixture should contain only migration 008's legacy seed group")

	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	afterCount, err := client.Group.Query().Count(seedCtx)
	require.NoError(t, err)
	require.Equal(t, beforeCount, afterCount, "initialized installation must not receive new default groups")
}

func TestEnsureSimpleModeDefaultGroups_BackfillsOnlyAutoCreatedGrokDefault(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	autoDefault, err := client.Group.Create().
		SetName(service.PlatformGrok + "-default").
		SetDescription("Auto-created default group").
		SetPlatform(service.PlatformGrok).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1.0).
		SetIsExclusive(false).
		SetAllowImageGeneration(false).
		Save(seedCtx)
	require.NoError(t, err)

	operatorGroup, err := client.Group.Create().
		SetName("operator-grok-images-disabled-" + time.Now().Format(time.RFC3339Nano)).
		SetDescription("Operator-managed group").
		SetPlatform(service.PlatformGrok).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1.0).
		SetIsExclusive(false).
		SetAllowImageGeneration(false).
		Save(seedCtx)
	require.NoError(t, err)

	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	autoDefault, err = client.Group.Get(seedCtx, autoDefault.ID)
	require.NoError(t, err)
	require.True(t, autoDefault.AllowImageGeneration)

	operatorGroup, err = client.Group.Get(seedCtx, operatorGroup.ID)
	require.NoError(t, err)
	require.False(t, operatorGroup.AllowImageGeneration, "operator-managed false must be preserved")
}

func TestEnsureSimpleModeDefaultGroups_PreservesExplicitFalse(t *testing.T) {
	tests := []struct {
		name        string
		description string
		status      string
	}{
		{
			name:        "operator managed default",
			description: "Operator-managed group",
			status:      service.StatusActive,
		},
		{
			name:        "disabled auto-created default",
			description: simpleModeDefaultGroupDescription,
			status:      service.StatusDisabled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			client := testEntTx(t).Client()
			resetSimpleModeDefaultGroupsFixture(t, client, ctx)
			grokDefault, err := client.Group.Create().
				SetName(service.PlatformGrok + "-default").
				SetDescription(tt.description).
				SetPlatform(service.PlatformGrok).
				SetStatus(tt.status).
				SetSubscriptionType(service.SubscriptionTypeStandard).
				SetRateMultiplier(1.0).
				SetIsExclusive(false).
				SetAllowImageGeneration(false).
				Save(ctx)
			require.NoError(t, err)

			require.NoError(t, ensureSimpleModeDefaultGroups(ctx, client))

			grokDefault, err = client.Group.Get(ctx, grokDefault.ID)
			require.NoError(t, err)
			require.False(t, grokDefault.AllowImageGeneration)
		})
	}
}

func TestEnsureSimpleModeDefaultGroups_PreservesSoftDeletedGroupHistory(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	// Create and then soft-delete an anthropic default group.
	g, err := client.Group.Create().
		SetName(service.PlatformAnthropic + "-default").
		SetPlatform(service.PlatformAnthropic).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1.0).
		SetIsExclusive(false).
		Save(seedCtx)
	require.NoError(t, err)

	_, err = client.Group.Delete().Where(group.IDEQ(g.ID)).Exec(seedCtx)
	require.NoError(t, err)

	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	// Soft-deleted history still proves that this is not a fresh installation.
	// Do not silently recreate a group the operator deliberately removed.
	count, err := client.Group.Query().Where(group.NameEQ(service.PlatformAnthropic+"-default"), group.DeletedAtIsNil()).Count(seedCtx)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestEnsureSimpleModeDefaultGroups_AntigravityNeedsTwoGroupsOnlyByCount(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	seedCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resetSimpleModeDefaultGroupsFixture(t, client, seedCtx)

	mustCreateGroup(t, client, &service.Group{Name: "ag-custom-1-" + time.Now().Format(time.RFC3339Nano), Platform: service.PlatformAntigravity})
	mustCreateGroup(t, client, &service.Group{Name: "ag-custom-2-" + time.Now().Format(time.RFC3339Nano), Platform: service.PlatformAntigravity})

	require.NoError(t, ensureSimpleModeDefaultGroups(seedCtx, client))

	count, err := client.Group.Query().Where(group.PlatformEQ(service.PlatformAntigravity), group.DeletedAtIsNil()).Count(seedCtx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, count, 2)
}
